package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"switchai/config"
	"switchai/history"
	"switchai/stats"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var (
	webPassword  string
	loginCookie  = "switchai_auth"
	sessionToken string // 存储当前会话 token，登录时生成，验证时比对
)

// SetPassword 设置 Web 管理界面密码
func SetPassword(pwd string) {
	webPassword = pwd
}

// 生成随机字符串
func generateRandomString(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)[:length]
}

//go:embed static/*
var staticFiles embed.FS

func RegisterRoutes(r *gin.Engine) {
	// Serve embedded static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}

	r.GET("/", func(c *gin.Context) {
		data, _ := staticFiles.ReadFile("static/index.html")
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	r.GET("/index.html", func(c *gin.Context) {
		data, _ := staticFiles.ReadFile("static/index.html")
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	r.GET("/log.html", func(c *gin.Context) {
		data, _ := staticFiles.ReadFile("static/log.html")
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	// Fallback for other static files if needed
	r.StaticFS("/static", http.FS(staticFS))

	// 登录 API (不需要认证)
	r.POST("/api/login", login)

	// 需要认证的 API 路由组
	api := r.Group("/api")
	api.Use(authMiddleware())
	{
		// 服务器密钥管理
		api.GET("/server-key", getServerKey)
		api.POST("/server-key/generate", generateServerKey)

		// 提供商管理
		api.GET("/providers", getProviders)
		api.POST("/providers", addProvider)
		api.PUT("/providers/:id", updateProvider)
		api.DELETE("/providers/:id", deleteProvider)
		api.POST("/providers/:id/activate", activateProvider)

		// 统计信息
		api.GET("/stats", getStats)
		api.POST("/stats/reset", resetStats)
		api.POST("/stats/reset/:provider_id", resetProviderStats)

		// 请求历史
		api.GET("/history", getHistory)
		api.GET("/history/:id", getHistoryDetail)

		// WebSocket
		api.GET("/ws", handleWebSocket)
	}
}

// authMiddleware 认证中间件，检查登录 cookie
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookieToken, err := c.Cookie(loginCookie)
		if err != nil || cookieToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录，请先登录"})
			c.Abort()
			return
		}

		// 验证 session token
		if cookieToken != sessionToken || sessionToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "会话无效，请重新登录"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// 生成会话 token
func generateSessionToken() string {
	return generateRandomString(32)
}

// login 处理登录
func login(c *gin.Context) {
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供密码"})
		return
	}

	if req.Password != webPassword {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "密码错误"})
		return
	}

	// 生成会话 token 并存储
	sessionToken = generateSessionToken()
	c.SetCookie(loginCookie, sessionToken, 86400, "/", "", false, true) // 24小时
	c.JSON(http.StatusOK, gin.H{"message": "登录成功"})
}

// getServerKey 获取当前服务器密钥
func getServerKey(c *gin.Context) {
	cfg := config.GetConfig()
	key := cfg.GetServerKey()
	c.JSON(http.StatusOK, gin.H{"server_key": key})
}

// generateServerKey 生成新的服务器密钥
func generateServerKey(c *gin.Context) {
	if err := config.GetConfig().GenerateServerKey(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成密钥失败"})
		return
	}
	key := config.GetConfig().GetServerKey()
	c.JSON(http.StatusOK, gin.H{"server_key": key, "message": "密钥已生成"})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	stats.GetStats().AddClient(conn)
	log.Println("New WebSocket client connected")

	// 发送当前统计数据
	summary := stats.GetStats().GetSummary()
	if err := conn.WriteJSON(summary); err != nil {
		log.Printf("Error sending initial stats: %v", err)
	}

	// 保持连接，等待客户端断开
	defer stats.GetStats().RemoveClient(conn)

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket client disconnected: %v", err)
			break
		}
	}
}

func getProviders(c *gin.Context) {
	cfg := config.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"providers":       cfg.Providers,
		"active_provider": cfg.ActiveProvider,
	})
}

func addProvider(c *gin.Context) {
	var provider config.Provider
	if err := c.ShouldBindJSON(&provider); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	provider.ID = uuid.New().String()
	provider.CreatedAt = time.Now().Format(time.RFC3339)

	if err := config.GetConfig().AddProvider(provider); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, provider)
}

func updateProvider(c *gin.Context) {
	id := c.Param("id")
	var provider config.Provider
	if err := c.ShouldBindJSON(&provider); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	provider.ID = id
	if err := config.GetConfig().UpdateProvider(id, provider); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, provider)
}

func deleteProvider(c *gin.Context) {
	id := c.Param("id")
	if err := config.GetConfig().DeleteProvider(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Provider deleted"})
}

func activateProvider(c *gin.Context) {
	id := c.Param("id")
	if err := config.GetConfig().SetActiveProvider(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Provider activated"})
}

func getStats(c *gin.Context) {
	summary := stats.GetStats().GetSummary()
	c.JSON(http.StatusOK, summary)
}

func resetStats(c *gin.Context) {
	stats.ResetStats()
	c.JSON(http.StatusOK, gin.H{"message": "All stats reset successfully"})
}

func resetProviderStats(c *gin.Context) {
	providerID := c.Param("provider_id")
	stats.ResetProviderStats(providerID)
	c.JSON(http.StatusOK, gin.H{"message": "Provider stats reset successfully"})
}

func getHistory(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	records, total := history.GetRecords(page, pageSize)
	c.JSON(http.StatusOK, gin.H{
		"records":   records,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func getHistoryDetail(c *gin.Context) {
	id := c.Param("id")
	record := history.GetRecord(id)
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Record not found"})
		return
	}
	c.JSON(http.StatusOK, record)
}
