package main

import (
    "bytes" // 【优化】用于高性能比较新旧配置内容
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

// 【优化】缓存上一份生成的配置内容，用于判断 configChanged，避免无意义 reload
var lastConfigBytes []byte

// 【新增】并发安全的用户流量缓存，避免 HTML 文件无意义写入导致 SSD 磨损
var lastUserBytes = struct {
    sync.RWMutex
    m map[string]int64
}{
    m: make(map[string]int64),
}

func initDB() {
    var err error
    // 连接 SQLite 数据库
    db, err = gorm.Open(sqlite.Open("data.db"), &gorm.Config{})
    if err != nil {
        log.Fatal("failed to connect database:", err)
    }

    // 【优化】开启 WAL 模式，提升并发读写性能（尤其是定时写流量时）
    if sqlDB, err := db.DB(); err == nil {
        _, _ = sqlDB.Exec("PRAGMA journal_mode=WAL;")
    }

    // 自动迁移表结构
    if err := db.AutoMigrate(&User{}); err != nil {
        log.Fatal("failed to migrate database:", err)
    }
}

func main() {
    // 设置 Gin HTTP 路由 (生产环境关闭 debug 模式可使用 gin.SetMode(gin.ReleaseMode))
    gin.SetMode(gin.ReleaseMode)

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
            var users []User
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

            // 【新增】清除缓存，确保下次会重写 HTML
            lastUserBytes.Lock()
            delete(lastUserBytes.m, req.Name)
            lastUserBytes.Unlock()

            c.JSON(200, gin.H{"status": "success"})
            go performCheckAndReload()
        })
    }

    fmt.Println("Backend is running on :8002")
    if err := r.Run("127.0.0.1:8002"); err != nil {
        log.Fatal("Gin server failed:", err)
    }
}

// 1. 定时去 sing-box 拉取增量流量
func fetchTrafficLoop() {
    // 【优化】将 gRPC 拨号移出循环，使用 grpc.Dial 建立长连接复用（兼容稍老版本的 grpc-go）
    conn, err := grpc.Dial("127.0.0.1:8001", grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        log.Fatalf("gRPC 初始化连接失败: %v", err)
    }
    defer conn.Close()

    client := command.NewStatsServiceClient(conn)

    for {
        time.Sleep(10 * time.Second) // 每 10 秒拉取一次

        // 【优化】增加超时控制，防止 sing-box 进程卡死时导致你的 Go 后端死锁
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        resp, err := client.QueryStats(ctx, &command.QueryStatsRequest{Pattern: "", Reset_: true})
        cancel()

        if err != nil {
            log.Println("获取流量失败(gRPC 稍后会自动重连):", err)
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

        var users []User
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

                // 【新增】清除缓存，确保 HTML 会重新生成
                lastUserBytes.Lock()
                delete(lastUserBytes.m, u.Name)
                lastUserBytes.Unlock()

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
    if err := json.Unmarshal(data, &config); err != nil {
        log.Println("模板 JSON 解析失败:", err)
        return
    }

    var usersInDB []User
    db.Find(&usersInDB)
    userMap := make(map[string]User)
    for _, u := range usersInDB {
        userMap[u.Name] = u
    }

    currentTime := time.Now().Unix()

    // 解析 inbounds 寻找 users 数组 (支持所有带有 users 的协议，如 vless, hysteria2 等)
    if inbounds, ok := config["inbounds"].([]interface{}); ok {
        for _, ib := range inbounds {
            inbound, ok := ib.(map[string]interface{})
            if !ok {
                continue
            }
            if usersInterface, hasUsers := inbound["users"]; hasUsers {
                rawUsers, ok := usersInterface.([]interface{})
                if !ok {
                    continue
                }
                var validUsers []interface{}

                for _, rawU := range rawUsers {
                    uMap, ok := rawU.(map[string]interface{})
                    if !ok {
                        continue
                    }
                    if nameObj, nameOk := uMap["name"]; nameOk {
                        name, ok := nameObj.(string)
                        if !ok {
                            continue
                        }

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

    // 【新增】为每个用户生成 HTML 文件（并发安全 + 防磨损）
    for _, u := range usersInDB {
        // 并发安全读取缓存
        lastUserBytes.RLock()
        last, ok := lastUserBytes.m[u.Name]
        lastUserBytes.RUnlock()

        // 如果流量没变 → 跳过写入，避免 SSD 磨损
        if ok && last == u.UsedBytes {
            continue
        }

        // 更新缓存
        lastUserBytes.Lock()
        lastUserBytes.m[u.Name] = u.UsedBytes
        lastUserBytes.Unlock()

        doubled := u.UsedBytes * 2
        gb := float64(doubled) / 1024 / 1024 / 1024

        // 完整 HTML 结构 + UTF-8 + 样式
        html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<title>流量查询</title>
<style>
body { font-family: Arial, sans-serif; text-align: center; margin-top: 40px; color: #333; }
</style>
</head>
<body>
您的已用流量为：%.2f GB
</body>
</html>`, gb)

        htmlPath := fmt.Sprintf("./%s.html", u.Name)
        if err := os.WriteFile(htmlPath, []byte(html), 0644); err != nil {
            log.Println("写入 HTML 文件失败:", err)
        }
    }

    // 无论是否改变，都重新生成一份 config.json 给 sing-box 使用
    newConfigBytes, err := json.MarshalIndent(config, "", "  ")
    if err != nil {
        log.Println("生成新配置 JSON 失败:", err)
        return
    }

    // 【优化】configChanged：如果新生成的配置和上一份完全一致，则不写文件、不 reload
    if lastConfigBytes != nil && bytes.Equal(lastConfigBytes, newConfigBytes) {
        return
    }
    lastConfigBytes = newConfigBytes

    // 【优化】使用临时文件 + 原子替换，避免 sing-box 读到半截文件
    tmpPath := "/etc/sing-box/config.json.tmp"
    finalPath := "/etc/sing-box/config.json"

    if err := os.WriteFile(tmpPath, newConfigBytes, 0644); err != nil {
        log.Println("写入临时 config.json 失败:", err)
        return
    }
    if err := os.Rename(tmpPath, finalPath); err != nil {
        log.Println("替换 config.json 失败:", err)
        return
    }

    // 简单粗暴，通知 systemd 热重载 sing-box
    if err := exec.Command("systemctl", "reload", "sing-box").Run(); err != nil {
        log.Println("重载 sing-box 失败:", err)
    }
}
