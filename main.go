package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/v2fly/v2ray-core/v5/app/stats/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/gorm"
)

// User 数据库模型
type User struct {
	Name           string `gorm:"primaryKey" json:"name"`
	UpBytes        int64  `json:"up_bytes"`
	DownBytes      int64  `json:"down_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	QuotaBytes     int64  `json:"quota_bytes"`
	ExpireTime     int64  `json:"expire_time"`
	ResetDay       int    `json:"reset_day"`        // 【新增】每月几号重置 (1-28，0代表不自动重置)
	LastResetMonth int    `json:"last_reset_month"` // 【新增】记录上次重置的月份，防止单日内重复清零
}

var db *gorm.DB
var mu sync.Mutex

func initDB() {
	var err error
	// 建议使用绝对路径，例如 "/etc/sing-box/data.db"
	db, err = gorm.Open(sqlite.Open("data.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("failed to connect database")
	}
	// 自动迁移表结构（会自动为你现有的数据库添加 reset_day 等新字段，不会丢失数据）
	db.AutoMigrate(&User{})
}

func main() {
	initDB()

	performCheckAndReload()

	go fetchTrafficLoop()
	go checkAndReloadLoop() // 里面包含了自动重置逻辑

	r := gin.Default()
	api := r.Group("/api")
	{
		// 获取用户列表
		api.GET("/users", func(c *gin.Context) {
			var users[]User
			db.Find(&users)
			c.JSON(200, users)
		})

		// 更新用户限额、到期时间、每月重置日
		api.POST("/users/update", func(c *gin.Context) {
			var req User
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "invalid format"})
				return
			}
			var user User
			if err := db.First(&user, "name = ?", req.Name).Error; err != nil {
				c.JSON(404, gin.H{"error": "user not found"})
				return
			}
			user.QuotaBytes = req.QuotaBytes
			user.ExpireTime = req.ExpireTime
			user.ResetDay = req.ResetDay // 保存自动重置日
			db.Save(&user)
			c.JSON(200, gin.H{"status": "success"})
			
			go performCheckAndReload()
		})

		// 【新增】手动清零某个用户的流量
		api.POST("/users/reset_traffic", func(c *gin.Context) {
			var req struct {
				Name string `json:"name"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(400, gin.H{"error": "invalid format"})
				return
			}
			
			// 将该用户的上下行和总流量清零
			db.Model(&User{}).Where("name = ?", req.Name).Updates(map[string]interface{}{
				"up_bytes":   0,
				"down_bytes": 0,
				"used_bytes": 0,
			})
			
			c.JSON(200, gin.H{"status": "success"})
			// 清零后，原本超流被封禁的用户应该恢复正常，所以需要立刻触发一次重载
			go performCheckAndReload()
		})
	}

	fmt.Println("Backend is running on :9090")
	r.Run("127.0.0.1:9090") 
}

func fetchTrafficLoop() {
	for {
		time.Sleep(10 * time.Second)
		conn, err := grpc.Dial("127.0.0.1:8080", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Println("gRPC 连接失败:", err)
			continue
		}
		client := command.NewStatsServiceClient(conn)
		resp, err := client.QueryStats(context.Background(), &command.QueryStatsRequest{Pattern: "", Reset: true})
		conn.Close()
		if err != nil {
			continue
		}

		for _, stat := range resp.Stat {
			parts := strings.Split(stat.Name, ">>>")
			if len(parts) >= 4 && parts[0] == "user" {
				username := parts[1]
				direction := parts[3]
				value := stat.Value

				if value > 0 {
					if direction == "uplink" {
						db.Model(&User{}).Where("name = ?", username).Updates(map[string]interface{}{
							"up_bytes":   gorm.Expr("up_bytes + ?", value),
							"used_bytes": gorm.Expr("used_bytes + ?", value),
						})
					} else if direction == "downlink" {
						db.Model(&User{}).Where("name = ?", username).Updates(map[string]interface{}{
							"down_bytes": gorm.Expr("down_bytes + ?", value),
							"used_bytes": gorm.Expr("used_bytes + ?", value),
						})
					}
				}
			}
		}
	}
}

func checkAndReloadLoop() {
	for {
		time.Sleep(1 * time.Minute)

		// 【新增】每月自动清零逻辑
		now := time.Now()
		currentDay := now.Day()
		currentMonth := int(now.Month())

		var users[]User
		db.Find(&users)
		for _, u := range users {
			// 如果今天刚好是设置的重置日，且本月还没重置过
			if u.ResetDay > 0 && u.ResetDay == currentDay && u.LastResetMonth != currentMonth {
				db.Model(&u).Updates(map[string]interface{}{
					"up_bytes":         0,
					"down_bytes":       0,
					"used_bytes":       0,
					"last_reset_month": currentMonth, // 标记本月已重置
				})
				log.Printf("用户 %s 达到每月重置日(%d号)，流量已自动清零\n", u.Name, u.ResetDay)
			}
		}

		// 检查超流/过期逻辑
		performCheckAndReload()
	}
}

func performCheckAndReload() {
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile("/etc/sing-box/config_template.json") 
	if err != nil {
		log.Println("无法读取 config_template.json:", err)
		return
	}

	var config map[string]interface{}
	json.Unmarshal(data, &config)

	var usersInDB[]User
	db.Find(&usersInDB)
	userMap := make(map[string]User)
	for _, u := range usersInDB {
		userMap[u.Name] = u
	}

	currentTime := time.Now().Unix()
	configChanged := false

	if inbounds, ok := config["inbounds"].([]interface{}); ok {
		for _, ib := range inbounds {
			inbound := ib.(map[string]interface{})
			if usersInterface, hasUsers := inbound["users"]; hasUsers {
				rawUsers := usersInterface.([]interface{})
				var validUsers[]interface{}

				for _, rawU := range rawUsers {
					uMap := rawU.(map[string]interface{})
					if nameObj, nameOk := uMap["name"]; nameOk {
						name := nameObj.(string)
						
						dbUser, exists := userMap[name]
						if !exists {
							dbUser = User{Name: name}
							db.Create(&dbUser)
							userMap[name] = dbUser
						}

						isExpired := dbUser.ExpireTime > 0 && currentTime > dbUser.ExpireTime
						isOverQuota := dbUser.QuotaBytes > 0 && dbUser.UsedBytes >= dbUser.QuotaBytes

						if isExpired || isOverQuota {
							log.Printf("阻断用户: %s (超流或过期)\n", name)
							configChanged = true
						} else {
							validUsers = append(validUsers, uMap) 
						}
					}
				}
				inbound["users"] = validUsers
			}
		}
	}

	newConfigBytes, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile("/etc/sing-box/config.json", newConfigBytes, 0644) 
	exec.Command("systemctl", "reload", "sing-box").Run()
}
