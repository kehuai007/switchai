package web

import (
	"embed"
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

	// API 路由
	api := r.Group("/api")
	{
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
