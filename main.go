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
	Name           string `gorm:"primaryKey" json:"name"` // 对应 sing-box users 里的 name
	UpBytes        int64  `json:"up_bytes"`               // 上传流量 (Byte)
	DownBytes      int64  `json:"down_bytes"`             // 下载流量 (Byte)
	UsedBytes      int64  `json:"used_bytes"`             // 合计总已用流量 (Byte)
	QuotaBytes     int64  `json:"quota_bytes"`            // 流量限额 (Byte)，0代表无限制
	ExpireTime     int64  `json:"expire_time"`            // 到期时间戳 (秒)，0代表无限制
	ResetDay       int    `json:"reset_day"`              // 【新增】每月几号重置 (1-28，0代表不自动重置)
	LastResetMonth int    `json:"last_reset_month"`       // 【新增】记录上次重置的月份，防止单日内重复清零
}

var db *gorm.DB
var mu sync.Mutex // 保护文件读写

func initDB() {
	var err error
	// 连接 SQLite 数据库
	db, err = gorm.Open(sqlite.Open("data.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("failed to connect database")
	}
	// 自动迁移表结构
	db.AutoMigrate(&User{})
}

func main() {
	initDB()

	// 启动时先执行一次检查并同步配置，确保数据库里有最新的用户数据
	performCheckAndReload()

	// 启动后台定时任务
	go fetchTrafficLoop()
	go checkAndReloadLoop()

	// 设置 Gin HTTP 路由 (生产环境关闭 debug 模式可使用 gin.SetMode(gin.ReleaseMode))
	r := gin.Default()
	api := r.Group("/api")
	{
		// 获取用户列表
		api.GET("/users", func(c *gin.Context) {
			var users[]User
			db.Find(&users)
			c.JSON(200, users)
		})

		// 更新用户限额或到期时间
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
			user.ResetDay = req.ResetDay // 【新增】保存自动重置日
			db.Save(&user)
			c.JSON(200, gin.H{"status": "success"})
			
			// 管理员修改规则后立即触发一次检查和重载，使其秒生效
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

	fmt.Println("Backend is running on :8002")
	// 监听本地 9090 端口，由 Caddy 反向代理
	r.Run("127.0.0.1:8002") 
}

// 1. 定时去 sing-box 拉取增量流量
func fetchTrafficLoop() {
	for {
		time.Sleep(10 * time.Second) // 每 10 秒拉取一次
		conn, err := grpc.Dial("127.0.0.1:8001", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Println("gRPC 连接失败，稍后重试:", err)
			continue
		}
		client := command.NewStatsServiceClient(conn)
		// 注意这里的 Reset_ : 因为 Go 中 Reset() 是结构体的内置方法，所以 pb 编译后字段名变成了 Reset_
		resp, err := client.QueryStats(context.Background(), &command.QueryStatsRequest{Pattern: "", Reset_: true})
		conn.Close()
		if err != nil {
			log.Println("获取流量失败:", err)
			continue
		}

		for _, stat := range resp.Stat {
			// stat.Name 格式为: user>>>用户名>>>traffic>>>uplink 或 downlink
			parts := strings.Split(stat.Name, ">>>")
			if len(parts) >= 4 && parts[0] == "user" {
				username := parts[1]
				direction := parts[3] // uplink 或 downlink
				value := stat.Value   // 最近10秒的增量 (Byte)

				if value > 0 {
					if direction == "uplink" {
						// 累加上传流量，同时累加总流量
						db.Model(&User{}).Where("name = ?", username).Updates(map[string]interface{}{
							"up_bytes":   gorm.Expr("up_bytes + ?", value),
							"used_bytes": gorm.Expr("used_bytes + ?", value),
						})
					} else if direction == "downlink" {
						// 累加下载流量，同时累加总流量
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

// 2. 定时检查超流/过期并重载配置文件
func checkAndReloadLoop() {
	for {
		time.Sleep(1 * time.Minute) // 每分钟例行检查一次

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

		// 执行核心阻断逻辑
		performCheckAndReload()
	}
}

// 核心阻断逻辑：合并模板和数据库状态
func performCheckAndReload() {
	mu.Lock()
	defer mu.Unlock()

	// 请确保此路径是你的模板文件路径
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

	// 解析 inbounds 寻找 users 数组 (支持所有带有 users 的协议，如 vless, hysteria2 等)
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
						
						// 如果数据库里没有这个用户，自动添加进去并初始化所有字段为 0
						dbUser, exists := userMap[name]
						if !exists {
							dbUser = User{Name: name, UpBytes: 0, DownBytes: 0, UsedBytes: 0, QuotaBytes: 0, ExpireTime: 0}
							db.Create(&dbUser)
							userMap[name] = dbUser
						}

						// 检查是否受限 (用总用量判断超流)
						isExpired := dbUser.ExpireTime > 0 && currentTime > dbUser.ExpireTime
						isOverQuota := dbUser.QuotaBytes > 0 && dbUser.UsedBytes >= dbUser.QuotaBytes

						if isExpired || isOverQuota {
							log.Printf("阻断用户: %s (超流或过期)\n", name)
						} else {
							// 只有合法的正常用户才被写入新配置
							validUsers = append(validUsers, uMap) 
						}
					}
				}
				inbound["users"] = validUsers
			}
		}
	}

	// 无论是否改变，都重新生成一份 config.json 给 sing-box 使用
	newConfigBytes, _ := json.MarshalIndent(config, "", "  ")
	
	// 请确保此路径是 sing-box 运行的实际配置文件路径
	os.WriteFile("/etc/sing-box/config.json", newConfigBytes, 0644) 

	// 简单粗暴，通知 systemd 热重载 sing-box (断开违规用户，正常用户不受影响)
	exec.Command("systemctl", "reload", "sing-box").Run()
}
