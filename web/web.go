package web

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"switchai/config"
	"switchai/history"
	"switchai/stats"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pquerna/otp/totp"
)

var (
	loginCookie = "switchai_auth"
)

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
	r.POST("/api/logout", logout)

	// 需要认证的 API 路由组
	api := r.Group("/api")
	api.Use(authMiddleware())
	{
		// 服务器密钥管理
		api.GET("/server-keys", getServerKeys)
		api.POST("/server-keys", addServerKey)
		api.PUT("/server-keys/:id", updateServerKey)
		api.DELETE("/server-keys/:id", deleteServerKey)
		api.POST("/server-keys/generate", generateServerKey)
		api.GET("/server-keys/:id/stats", getServerKeyStats)
		api.POST("/server-keys/:id/test", testServerKey)
		api.GET("/server-keys/:id/mappings", getKeyMappings)
		api.POST("/server-keys/:id/mappings", addKeyMapping)
		api.PUT("/server-keys/:id/mappings/:mapping_id", updateKeyMapping)
		api.DELETE("/server-keys/:id/mappings/:mapping_id", deleteKeyMapping)

		// 提供商管理
		api.GET("/providers", getProviders)
		api.POST("/providers", addProvider)
		api.PUT("/providers/:id", updateProvider)
		api.DELETE("/providers/:id", deleteProvider)
		api.POST("/providers/:id/activate", activateProvider)
		api.POST("/providers/:id/test", testProvider)
		api.POST("/providers/fetch-models", fetchModelsByCredentials)
		api.POST("/providers/:id/fetch-models", fetchProviderModels)

		// 统计信息
		api.GET("/stats", getStats)
		api.GET("/stats/daily", getDailyStats)
		api.POST("/stats/reset", resetStats)
		api.POST("/stats/reset/:provider_id", resetProviderStats)

		// 请求历史
		api.GET("/history", getHistory)
		api.GET("/history/:id", getHistoryDetail)

		// WebSocket
		api.GET("/ws", handleWebSocket)
		api.GET("/ws/history", handleHistoryWebSocket)
		api.GET("/ws/config", handleConfigWebSocket)
	}

	// 2FA 相关 API（不需要认证）
	r.POST("/api/totp/setup", totpSetup)
	r.POST("/api/totp/verify", totpVerify)
	r.GET("/api/totp/status", totpStatus)
}

// authMiddleware 认证中间件，检查登录 cookie
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// skip 模式下跳过认证
		if config.IsSkipAuth() {
			c.Next()
			return
		}

		cookieToken, err := c.Cookie(loginCookie)
		if err != nil || cookieToken == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录，请先登录"})
			c.Abort()
			return
		}

		cfg := config.GetConfig()

		// 验证 session token（多端登录支持）
		if !cfg.ValidateSessionToken(cookieToken) {
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

// login 处理登录（首次访问显示2FA设置，之后验证2FA）
func login(c *gin.Context) {
	var req struct {
		Code string `json:"code"` // 2FA 验证码
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供验证码"})
		return
	}

	cfg := config.GetConfig()

	// 检查是否已设置 TOTP
	if !cfg.IsTOTPEnabled() {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先设置2FA"})
		return
	}

	// 验证 TOTP 验证码
	if !totp.Validate(req.Code, cfg.GetTOTPSecret()) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "验证码错误"})
		return
	}

	// 验证成功，生成会话 token 并添加到列表（支持多端登录）
	sessionToken := generateSessionToken()
	if err := cfg.AddSessionToken(sessionToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存会话失败"})
		return
	}
	c.SetCookie(loginCookie, sessionToken, 0, "/", "", false, true) // 永不过期
	c.JSON(http.StatusOK, gin.H{"message": "登录成功"})
}

// totpSetup 首次设置 TOTP（生成密钥和二维码）
func totpSetup(c *gin.Context) {
	cfg := config.GetConfig()

	// 如果已经启用，不允许重新设置
	if cfg.IsTOTPEnabled() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "2FA已启用，如需重置请删除配置文件"})
		return
	}

	// 生成新的 TOTP 密钥
	secret := cfg.GetTOTPSecret()
	if secret == "" {
		// 生成随机密钥
		key, err := totp.Generate(totp.GenerateOpts{
			Issuer:      "SwitchAI",
			AccountName: "admin",
			Period:      30,
			SecretSize:  20,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "生成2FA密钥失败"})
			return
		}
		secret = key.Secret()
		if err := cfg.SetTOTPSecret(secret); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存2FA密钥失败"})
			return
		}
	}

	// 生成二维码 URL
	otpURL := fmt.Sprintf("otpauth://totp/SwitchAI:admin?secret=%s&issuer=SwitchAI&period=30", secret)

	c.JSON(http.StatusOK, gin.H{
		"secret": secret,
		"otpauth": otpURL,
	})
}

// logout 处理退出登录（只清除当前 token，不影响其他设备）
func logout(c *gin.Context) {
	cookieToken, _ := c.Cookie(loginCookie)
	if cookieToken != "" {
		config.GetConfig().RemoveSessionToken(cookieToken)
	}
	c.SetCookie(loginCookie, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "已退出登录"})
}

// totpVerify 验证 TOTP 验证码（绑定时使用）
func totpVerify(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供验证码"})
		return
	}

	cfg := config.GetConfig()
	secret := cfg.GetTOTPSecret()

	if secret == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先设置2FA"})
		return
	}

	// 验证验证码
	if !totp.Validate(req.Code, secret) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "验证码错误"})
		return
	}

	// 验证成功，启用 TOTP
	if err := cfg.EnableTOTP(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "启用2FA失败"})
		return
	}

	// 生成会话 token 并添加到列表
	sessionToken := generateSessionToken()
	if err := cfg.AddSessionToken(sessionToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存会话失败"})
		return
	}
	c.SetCookie(loginCookie, sessionToken, 0, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "2FA绑定成功"})
}

// totpStatus 获取 TOTP 状态
func totpStatus(c *gin.Context) {
	cfg := config.GetConfig()
	c.JSON(http.StatusOK, gin.H{
		"enabled":    cfg.IsTOTPEnabled(),
		"has_secret": cfg.GetTOTPSecret() != "",
	})
}

// getServerKeys 获取所有服务器密钥
func getServerKeys(c *gin.Context) {
	cfg := config.GetConfig()
	keys := cfg.GetServerKeys()
	c.JSON(http.StatusOK, gin.H{"server_keys": keys})
}

// addServerKey 添加服务器密钥
func addServerKey(c *gin.Context) {
	var key config.ServerKey
	if err := c.ShouldBindJSON(&key); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 生成随机密钥值
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成密钥失败"})
		return
	}
	keyStr := "sk-"
	for _, b := range bytes {
		keyStr += string(chars[int(b)%len(chars)])
	}
	key.Key = keyStr
	key.IsEnabled = true

	if err := config.GetConfig().AddServerKey(key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "key_added", key.ID)
	c.JSON(http.StatusOK, gin.H{"message": "密钥添加成功", "key": key})
}

// updateServerKey 更新服务器密钥
func updateServerKey(c *gin.Context) {
	id := c.Param("id")
	var key config.ServerKey
	if err := c.ShouldBindJSON(&key); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := config.GetConfig().UpdateServerKey(id, key); err != nil {
		if errors.Is(err, config.ErrServerKeyDuplicate) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "key_updated", id)
	c.JSON(http.StatusOK, gin.H{"message": "密钥更新成功"})
}

// deleteServerKey 删除服务器密钥
func deleteServerKey(c *gin.Context) {
	id := c.Param("id")
	if err := config.GetConfig().DeleteServerKey(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "key_deleted", id)
	c.JSON(http.StatusOK, gin.H{"message": "密钥删除成功"})
}

// generateServerKey 生成新的服务器密钥
func generateServerKey(c *gin.Context) {
	keyStr, err := config.GetConfig().GenerateServerKeyString()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成密钥失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"server_key": keyStr, "message": "密钥已生成"})
}

// getServerKeyStats 获取指定密钥的统计信息
func getServerKeyStats(c *gin.Context) {
	id := c.Param("id")
	keyStat := stats.GetKeyStats(id)
	if keyStat == nil {
		c.JSON(http.StatusOK, gin.H{
			"key_id":       id,
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens": 0,
			"ip_addresses": []string{},
			"request_count": 0,
		})
		return
	}
	c.JSON(http.StatusOK, keyStat)
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

func handleHistoryWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	history.AddClient(conn)
	log.Println("New history WebSocket client connected")

	// 发送最近 20 条历史记录
	records, total := history.GetRecordsSummary(1, 20)
	if err := conn.WriteJSON(gin.H{"type": "history", "records": records, "total": total}); err != nil {
		log.Printf("Error sending initial history: %v", err)
	}

	// 保持连接，等待客户端断开
	defer history.RemoveClient(conn)

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("History WebSocket client disconnected: %v", err)
			break
		}
	}
}

func getProviders(c *gin.Context) {
	cfg := config.GetConfig()
	// 不返回实际的 API Key，保护密钥安全
	providers := make([]config.Provider, len(cfg.Providers))
	for i, p := range cfg.Providers {
		providers[i] = p
		providers[i].APIKey = "" // 返回空，避免泄露
	}
	c.JSON(http.StatusOK, gin.H{
		"providers": providers,
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
	if !provider.IsActive {
		provider.IsActive = true // 新建默认激活
	}

	if err := config.GetConfig().AddProvider(provider); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "provider_added", provider.ID)
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
	// 如果 api_key 为空，保留原值
	if provider.APIKey == "" {
		oldProvider := config.GetConfig().GetProviderByID(id)
		if oldProvider != nil {
			provider.APIKey = oldProvider.APIKey
			provider.IsActive = oldProvider.IsActive // 保留原激活状态
		}
	}

	if err := config.GetConfig().UpdateProvider(id, provider); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "provider_updated", id)
	c.JSON(http.StatusOK, provider)
}

func deleteProvider(c *gin.Context) {
	id := c.Param("id")
	hasMappings, err := config.GetConfig().HasMappingsForProvider(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if hasMappings {
		c.JSON(http.StatusConflict, gin.H{"error": "provider has active mapping(s); delete them first"})
		return
	}
	if err := config.GetConfig().DeleteProvider(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	broadcastConfigChange(c, "provider_deleted", id)
	c.JSON(http.StatusOK, gin.H{"message": "Provider deleted"})
}

func activateProvider(c *gin.Context) {
	id := c.Param("id")
	cfg := config.GetConfig()

	provider := cfg.GetProviderByID(id)
	if provider == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}

	provider.IsActive = !provider.IsActive
	if err := cfg.UpdateProvider(id, *provider); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "provider_activated", id)
	c.JSON(http.StatusOK, gin.H{
		"message":   "Provider activation toggled",
		"is_active": provider.IsActive,
	})
}

// testProvider 测试提供商连接，发送 "hi" 消息
func testProvider(c *gin.Context) {
	id := c.Param("id")
	cfg := config.GetConfig()

	provider := cfg.GetProviderByID(id)
	if provider == nil {
		log.Printf("❌ testProvider: provider not found, id=%s", id)
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}

	// 解析请求体：可选的 model + is_openai_format 字段。
	// model：前端让用户在 supported_models 列表里挑一个来测试。
	// is_openai_format：前端实时读"API 格式"下拉框的值，覆盖 DB 里的 provider.IsOpenAIFormat；
	// 用户切换下拉后立即影响本次测试，不必先保存。
	var body struct {
		Model          string `json:"model"`
		IsOpenAIFormat *bool  `json:"is_openai_format,omitempty"`
	}
	_ = c.ShouldBindJSON(&body) // body 可为空

	// 测试时用的协议格式：请求体显式传了就用请求体的，否则回落到 provider DB 里的值
	isOpenAIFormat := provider.IsOpenAIFormat
	if body.IsOpenAIFormat != nil {
		isOpenAIFormat = *body.IsOpenAIFormat
	}

	// 确定测试用的模型：优先用请求体里的 model，否则从支持的模型里取第一个。
	// provider.Model 字段存储的是分号分隔的多模型名单（如 "X;Y;Z"），不能直接当单模型发给上游。
	testModel := strings.TrimSpace(body.Model)
	if testModel == "" {
		models := provider.GetSupportedModels()
		if len(models) > 0 {
			testModel = models[0]
		} else {
			testModel = strings.TrimSpace(provider.Model)
		}
	}
	if testModel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no model available to test (provider has no models configured)"})
		return
	}

	log.Printf("🔍 testProvider: id=%s, name=%s, baseURL=%s, isOpenAI=%v, testModel=%s",
		id, provider.Name, provider.BaseURL, isOpenAIFormat, testModel)

	// 根据提供商格式构建测试请求
	var reqBody []byte
	var targetURL string

	if isOpenAIFormat {
		// OpenAI 格式
		openAIReq := map[string]interface{}{
			"model": testModel,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "你是什么模型"},
			},
			// 测试只是要确认连通+看到完整回复，10 太短会触发 finish_reason=length 让 AI 答一半就停。
			"max_tokens": 256,
		}
		reqBody, _ = json.Marshal(openAIReq)
		targetURL = config.BuildProviderURL(provider.BaseURL, "/chat/completions")
		log.Printf("🔗 OpenAI format, targetURL: %s", targetURL)
	} else {
		// Claude 格式
		claudeReq := map[string]interface{}{
			"model": testModel,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "你是什么模型"},
			},
			"max_tokens": 256,
		}
		reqBody, _ = json.Marshal(claudeReq)
		targetURL = config.BuildProviderURL(provider.BaseURL, "/messages")
		log.Printf("🔗 Claude format, targetURL: %s", targetURL)
	}

	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("❌ testProvider: failed to create request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if !isOpenAIFormat {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	log.Printf("📤 testProvider: sending request to %s", targetURL)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ testProvider: request failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Connection failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	maxLen := 200
	if len(respBody) < maxLen {
		maxLen = len(respBody)
	}
	log.Printf("📥 testProvider: response status=%d, body=%s", resp.StatusCode, string(respBody[:maxLen]))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 解析响应，提取 AI 的回复内容
		var aiReply string
		if provider.IsOpenAIFormat {
			// OpenAI 格式响应
			var openaiResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if json.Unmarshal(respBody, &openaiResp) == nil && len(openaiResp.Choices) > 0 {
				aiReply = openaiResp.Choices[0].Message.Content
			}
		} else {
			// Claude 格式响应
			var claudeResp struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(respBody, &claudeResp) == nil && len(claudeResp.Content) > 0 {
				aiReply = claudeResp.Content[0].Text
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"status":   resp.StatusCode,
			"message":  "Connection successful",
			"response": string(respBody),
			"aiReply":  aiReply,
		})
	} else {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"status":  resp.StatusCode,
			"message": "Connection failed",
			"response": string(respBody),
		})
	}
}

func testServerKey(c *gin.Context) {
	keyID := c.Param("id")

	var req struct {
		ProviderType  string `json:"provider_type"`  // "anthropic" 或 "openai"
		ProviderID    string `json:"provider_id"`
		ProviderModel string `json:"provider_model"` // 仅用于日志/校验，真正请求体里用的是 user_model
		UserModel     string `json:"user_model"`     // 用户原始请求的模型名（映射的 user_model 字段）
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	if req.ProviderID == "" || req.ProviderModel == "" || req.UserModel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider_id, provider_model and user_model required"})
		return
	}

	// 根据选择的格式确定使用哪个提供商进行测试
	isOpenAIFormat := req.ProviderType == "openai"

	cfg := config.GetConfig()

	// 获取要测试的服务器密钥
	serverKey := cfg.GetServerKeyByID(keyID)
	if serverKey == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Server key not found"})
		return
	}

	// 使用请求指定的提供商和模型进行测试（按真实用户路径走代理路由，让代理把 user_model 替换成 provider_model）
	provider := cfg.GetProviderByID(req.ProviderID)
	if provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Provider not found"})
		return
	}

	log.Printf("🔍 Testing server-key: %s, user_model: %s → provider: %s (format: %s, key type: %s)",
		keyID, req.UserModel, provider.Name, req.ProviderType, serverKey.Key[:12]+"...")

	// 构建测试请求，发送到本服务的代理端点
	var reqBody []byte
	var targetURL string

	// 获取本服务地址
	baseURL := "http://" + c.Request.Host

	// 按真实客户端的方式发送：用 user_model 作为请求体 model，代理会根据该 key 的映射把它替换成 provider_model 并转发。
	if isOpenAIFormat {
		openAIReq := map[string]interface{}{
			"model": req.UserModel,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "你是什么模型"},
			},
			"max_tokens": 256,
		}
		reqBody, _ = json.Marshal(openAIReq)
		targetURL = baseURL + "/v1/chat/completions"
	} else {
		claudeReq := map[string]interface{}{
			"model": req.UserModel,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "你是什么模型"},
			},
			"max_tokens": 256,
		}
		reqBody, _ = json.Marshal(claudeReq)
		targetURL = baseURL + "/v1/messages"
	}

	log.Printf("🔗 Test request URL: %s", targetURL)
	log.Printf("📤 Test request body: %s", string(reqBody))

	testReq, err := http.NewRequest("POST", targetURL, bytes.NewReader(reqBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 使用服务器密钥进行授权
	testReq.Header.Set("Authorization", "Bearer "+serverKey.Key)
	testReq.Header.Set("Content-Type", "application/json")
	if !isOpenAIFormat {
		testReq.Header.Set("anthropic-version", "2023-06-01")
	}

	// 记录请求详情（key 只显示前后各4位）
	maskedKey := serverKey.Key
	if len(maskedKey) > 8 {
		maskedKey = maskedKey[:4] + "..." + maskedKey[len(maskedKey)-4:]
	}
	log.Printf("📡 Request details - BaseURL: %s, Format: %s, Key: %s", baseURL, req.ProviderType, maskedKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(testReq)
	if err != nil {
		log.Printf("❌ Server-key test connection failed: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Connection failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("📥 Test response - Status: %d, Body: %s", resp.StatusCode, string(respBody))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var aiReply string
		// 根据选择的格式解析响应
		if isOpenAIFormat {
			var openaiResp struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if json.Unmarshal(respBody, &openaiResp) == nil && len(openaiResp.Choices) > 0 {
				aiReply = openaiResp.Choices[0].Message.Content
			}
		} else {
			var claudeResp struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(respBody, &claudeResp) == nil && len(claudeResp.Content) > 0 {
				aiReply = claudeResp.Content[0].Text
			}
		}

		log.Printf("✅ Server-key test successful via provider: %s", provider.Name)
		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"status":       resp.StatusCode,
			"message":      "Connection successful",
			"response":     string(respBody),
			"aiReply":      aiReply,
			"providerName": provider.Name,
		})
	} else {
		log.Printf("❌ Server-key test failed - Status: %d, Response: %s", resp.StatusCode, string(respBody))
		c.JSON(http.StatusOK, gin.H{
			"success":      false,
			"status":       resp.StatusCode,
			"message":      "Connection failed",
			"response":     string(respBody),
			"providerName": provider.Name,
		})
	}
}

func getStats(c *gin.Context) {
	summary := stats.GetStats().GetSummary()
	c.JSON(http.StatusOK, summary)
}

func getDailyStats(c *gin.Context) {
	s := stats.GetStats()
	today := s.GetTodaySummary()
	dailyHistory := s.GetDailyHistory()
	c.JSON(http.StatusOK, gin.H{
		"today":        today,
		"daily_history": dailyHistory,
	})
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

	records, total := history.GetRecordsSummary(page, pageSize)
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

// getKeyMappings 获取指定密钥的所有模型映射
func getKeyMappings(c *gin.Context) {
	keyID := c.Param("id")
	mappings := config.GetConfig().LoadMappingsForKey(keyID)
	if mappings == nil {
		mappings = []config.ModelMapping{}
	}
	c.JSON(http.StatusOK, gin.H{"mappings": mappings})
}

// addKeyMapping 为指定密钥添加模型映射
func addKeyMapping(c *gin.Context) {
	keyID := c.Param("id")
	var m config.ModelMapping
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if m.UserModel == "" || m.ProviderID == "" || m.ProviderModel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "required field missing"})
		return
	}
	if config.GetConfig().GetProviderByID(m.ProviderID) == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found"})
		return
	}

	created, err := config.GetConfig().AddMapping(keyID, m)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			c.JSON(http.StatusConflict, gin.H{"error": "duplicate user model name"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "mapping_added", created.ID)
	c.JSON(http.StatusOK, created)
}

// updateKeyMapping 更新指定密钥的某个模型映射
func updateKeyMapping(c *gin.Context) {
	keyID := c.Param("id")
	mappingID := c.Param("mapping_id")
	var m config.ModelMapping
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if m.UserModel == "" || m.ProviderID == "" || m.ProviderModel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "required field missing"})
		return
	}
	if config.GetConfig().GetProviderByID(m.ProviderID) == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider not found"})
		return
	}

	if err := config.GetConfig().UpdateMapping(keyID, mappingID, m); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	broadcastConfigChange(c, "mapping_updated", mappingID)
	c.JSON(http.StatusOK, gin.H{"message": "mapping updated"})
}

// deleteKeyMapping 删除指定密钥的某个模型映射
func deleteKeyMapping(c *gin.Context) {
	keyID := c.Param("id")
	mappingID := c.Param("mapping_id")
	if err := config.GetConfig().DeleteMapping(keyID, mappingID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	broadcastConfigChange(c, "mapping_deleted", mappingID)
	c.JSON(http.StatusOK, gin.H{"message": "mapping deleted"})
}

// fetchOpenAIModels 调用 OpenAI 兼容的 /v1/models 接口
func fetchOpenAIModels(modelsURL, apiKey string) ([]string, error) {
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	var models []string
	for _, m := range parsed.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// resolveModels 调用 provider 的 /v1/models 接口；任何错误都向上传播，不内置 fallback。
// 上游是否真的暴露 /v/models 取决于用户配置 —— 协议层的可行性由 isOpenAIFormat 之外的
// 实际部署决定，本函数只负责 URL 拼接（复用 config.BuildProviderURL 避免 /v1/v1 或 //）。
func resolveModels(baseURL, apiKey string) ([]string, error) {
	modelsURL := config.BuildProviderURL(baseURL, "/models")
	return fetchOpenAIModels(modelsURL, apiKey)
}

// fetchProviderModels 处理 POST /api/providers/:id/fetch-models — 编辑弹窗用
func fetchProviderModels(c *gin.Context) {
	id := c.Param("id")
	cfg := config.GetConfig()
	provider := cfg.GetProviderByID(id)
	if provider == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Provider not found"})
		return
	}

	models, err := resolveModels(provider.BaseURL, provider.APIKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	joined := strings.Join(models, ";")
	c.JSON(http.StatusOK, gin.H{"models": joined, "count": len(models)})
}

// fetchModelsByCredentials 处理 POST /api/providers/fetch-models — 添加弹窗用
// body: { base_url, api_key, is_openai_format }
func fetchModelsByCredentials(c *gin.Context) {
	var req struct {
		BaseURL        string `json:"base_url"`
		APIKey         string `json:"api_key"`
		IsOpenAIFormat bool   `json:"is_openai_format"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.BaseURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "base_url required"})
		return
	}

	models, err := resolveModels(req.BaseURL, req.APIKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	joined := strings.Join(models, ";")
	c.JSON(http.StatusOK, gin.H{"models": joined, "count": len(models)})
}
