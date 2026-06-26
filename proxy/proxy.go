package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"switchai/config"
	"switchai/history"
	"switchai/logger"
	"switchai/stats"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func RegisterRoutes(r *gin.Engine) {
	// 代理所有 /v1/* 路径
	// 注意：/v1/models 也走这里，由 proxyHandler 顶部特判分发 —— gin v1.10 的
	// radix tree 不允许 "/v1/models" 静态路由与 "/v1/*path" 通配符共存。
	r.Any("/v1/*path", proxyHandler)
}

// openAIModelEntry 是 OpenAI /v1/models 单条记录的 JSON 形状。
// 字段名（id / object / created / owned_by）固定：客户端按这些名字解析。
type openAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// openAIModelsList 是 OpenAI /v1/models 顶层响应的 JSON 形状。
type openAIModelsList struct {
	Object string            `json:"object"`
	Data   []openAIModelEntry `json:"data"`
}

// buildModelsListResponse 把该 key 可见的 active 映射转成 OpenAI /v1/models 响应。
//   - 取 UserModel（客户端能直接调用的名字）作为 id —— 不是 ProviderModel（上游实际名）；
//     客户端拿到列表后用它去调网关，网关再去路由到对应的 provider_model；
//   - 同一 user_model 不会重复（DB 唯一约束保证，但仍去重保险）；
//   - 按 id 排序，让客户端能依赖稳定顺序；
//   - data 始终返回非 nil 空 slice，handler 序列化时输出 [] 而非 null。
func buildModelsListResponse(mappings []config.ModelMapping) openAIModelsList {
	seen := make(map[string]struct{}, len(mappings))
	for _, m := range mappings {
		if m.UserModel == "" {
			continue
		}
		seen[m.UserModel] = struct{}{}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	now := time.Now().Unix()
	data := make([]openAIModelEntry, 0, len(ids))
	for _, id := range ids {
		data = append(data, openAIModelEntry{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "switchai",
		})
	}
	return openAIModelsList{Object: "list", Data: data}
}

// listModelsForKey 处理 GET /v1/models — 按 Bearer key 返回该 key 可见的、目标 provider
// 处于 active 状态的映射目标模型。响应形状兼容 OpenAI /v1/models。
func listModelsForKey(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization header"})
		return
	}
	providedKey := authHeader
	if strings.HasPrefix(authHeader, "Bearer ") {
		providedKey = strings.TrimPrefix(authHeader, "Bearer ")
	}
	keyID, isValid := config.GetConfig().ValidateServerKey(providedKey)
	if !isValid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or disabled server key"})
		return
	}

	mappings := config.GetConfig().GetActiveMappingsForKey(keyID)
	c.JSON(http.StatusOK, buildModelsListResponse(mappings))
}

// doRequestWithRetry 执行请求并在遇到 "try again" 错误时自动重试
// finalAttempt 是最后一次执行的 attempt 编号（从 1 开始）。调用方用 finalAttempt-1 得到额外重试次数。
func doRequestWithRetry(req *http.Request, bodyBytes []byte, provider *config.Provider, maxRetries int) (resp *http.Response, finalAttempt int, err error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		finalAttempt = attempt
		// 为每次重试创建新的请求体 reader
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				logger.Info("⚠️ 请求失败 (尝试 %d/%d): %v，准备重试...", attempt, maxRetries, err)
				time.Sleep(time.Duration(attempt) * time.Second) // 递增延迟
				continue
			}
			return nil, attempt, err
		}

		// 读取响应体以检查是否包含 "try again" 错误
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				logger.Info("⚠️ 读取响应失败 (尝试 %d/%d): %v，准备重试...", attempt, maxRetries, err)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return nil, attempt, err
		}

		// 检查响应是否包含需要重试的错误
		shouldRetry := false
		if resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 529 {
			var errorResp map[string]interface{}
			if json.Valid(respBody) && json.Unmarshal(respBody, &errorResp) == nil {
				if errorObj, ok := errorResp["error"].(map[string]interface{}); ok {
					// 检查 error type
					if errorType, ok := errorObj["type"].(string); ok {
						if errorType == "overloaded_error" || errorType == "rate_limit_error" {
							shouldRetry = true
						}
					}
					// 检查错误消息
					if message, ok := errorObj["message"].(string); ok {
						lowerMsg := strings.ToLower(message)
						if strings.Contains(lowerMsg, "try again") ||
							strings.Contains(lowerMsg, "high traffic") ||
							strings.Contains(lowerMsg, "overloaded") ||
							strings.Contains(lowerMsg, "负载较高") {
							shouldRetry = true
						}
					}
				}
			}
		}

		// 如果需要重试且还有重试次数
		if shouldRetry && attempt < maxRetries {
			logger.Info("⚠️ 检测到需重试错误 (尝试 %d/%d)：status=%d, type=overloaded_error，准备重试...", attempt, maxRetries, resp.StatusCode)
			time.Sleep(time.Duration(attempt) * time.Second) // 递增延迟
			continue
		}

		// 成功或不需要重试，重新包装响应体并返回
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if shouldRetry && attempt == maxRetries {
			logger.Info("❌ 重试 %d 次后仍然失败，终止重试", maxRetries)
		} else if attempt > 1 {
			logger.Info("✅ 重试成功 (尝试 %d/%d)", attempt, maxRetries)
		}
		return resp, attempt, nil
	}

	return lastResp, maxRetries, lastErr
}

// resolveRouteTarget 根据 keyID + userModel 解析出真正的目标 provider 与其下模型名
// 返回 (*Provider, provider_model, error)
func resolveRouteTarget(keyID, userModel string) (*config.Provider, string, error) {
	mapping, provider, err := config.GetConfig().GetMappingForRouting(keyID, userModel)
	if err != nil {
		return nil, "", err
	}
	return provider, mapping.ProviderModel, nil
}

func proxyHandler(c *gin.Context) {
	startTime := time.Now()
	requestID := uuid.New().String()

	// GET /v1/models 在此特判分发；不能挂 r.GET("/v1/models", ...) 因为 gin v1.10
	// radix tree 不允许与 r.Any("/v1/*path", ...) 通配共存（启动会 panic）。
	if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/models" {
		listModelsForKey(c)
		return
	}

	// 验证服务器密钥
	authHeader := c.GetHeader("Authorization")
	if authHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing authorization header"})
		return
	}

	// 支持 "Bearer sk-xxxx" 或 "sk-xxxx" 格式
	providedKey := authHeader
	if strings.HasPrefix(authHeader, "Bearer ") {
		providedKey = strings.TrimPrefix(authHeader, "Bearer ")
	}

	// 验证密钥并获取密钥ID
	keyID, isValid := config.GetConfig().ValidateServerKey(providedKey)
	if !isValid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid or disabled server key"})
		return
	}

	// 检查密钥限额
	serverKey := config.GetConfig().GetServerKeyByID(keyID)
	if serverKey != nil {
		allowed, reason := stats.CheckKeyLimit(keyID,
			serverKey.DailyReqLimit, serverKey.TotalReqLimit,
			serverKey.DailyCostLimit, serverKey.TotalCostLimit)
		if !allowed {
			c.JSON(http.StatusForbidden, gin.H{"error": reason})
			return
		}
	}

	// 检测请求格式：Anthropic 使用 /v1/messages，OpenAI 使用 /v1/chat/completions
	isIncomingOpenAIFormat := strings.HasPrefix(c.Request.URL.Path, "/v1/chat")

	// 解析请求体以取出 user model（先 read body）
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	var requestedModel string
	if len(bodyBytes) > 0 {
		var probe map[string]interface{}
		if json.Unmarshal(bodyBytes, &probe) == nil {
			if m, ok := probe["model"].(string); ok {
				requestedModel = m
			}
		}
	}

	// userModel 保留用户原始请求的模型名（mapping 之前的值），用于历史记录
	userModel := requestedModel

	// 严格模式：通过 key + user_model 路由
	provider, providerModel, err := resolveRouteTarget(keyID, userModel)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, config.ErrConfiguredProviderMissing) {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	clientIP := c.ClientIP()

	// 构建目标 URL
	// 如果 BaseURL 已经是完整路径（包含 /v1/），只替换路径部分；否则拼接路径
	targetURL := provider.BaseURL
	if strings.Contains(targetURL, "/v1/") {
		// BaseURL 包含 /v1/，可能是完整路径，需要替换掉后面的部分
		idx := strings.Index(targetURL, "/v1/")
		targetURL = targetURL[:idx] + c.Request.URL.Path
		if c.Request.URL.RawQuery != "" {
			targetURL += "?" + c.Request.URL.RawQuery
		}
	} else {
		// BaseURL 不包含 /v1/，正常拼接
		targetURL = strings.TrimSuffix(targetURL, "/") + c.Request.URL.Path
		if c.Request.URL.RawQuery != "" {
			targetURL += "?" + c.Request.URL.RawQuery
		}
	}
	logger.Info("📡 代理转发 - Provider: %s, BaseURL: %s, Path: %s → Target: %s", provider.Name, provider.BaseURL, c.Request.URL.Path, targetURL)

	// 解析请求体以检查是否为流式请求，并替换模型参数
	var requestBody map[string]interface{}
	isStream := false
	requestedModel = "unknown"
	actualModel := providerModel
	modifiedRequestBody := string(bodyBytes) // 用于历史记录的请求体

	if len(bodyBytes) > 0 && json.Valid(bodyBytes) {
		if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
			// 检查是否为流式请求
			if stream, ok := requestBody["stream"].(bool); ok && stream {
				isStream = true
			}

			// 获取用户原始模型名（用于日志/历史）
			if model, ok := requestBody["model"].(string); ok {
				requestedModel = model
				logger.Info("Original request model: %s", model)
			}

			// 使用映射解析出的 provider_model 替换请求中的模型
			// actualModel 用于 cost 计算，requestedModel 保留用户原始输入（用于 history.user_model）
			requestBody["model"] = providerModel
			actualModel = providerModel
			logger.Info("Replaced with provider model: %s", providerModel)

			// 自动格式转换：如果请求格式与提供商格式不匹配，需要转换
			if provider.IsOpenAIFormat && !isIncomingOpenAIFormat {
				// 请求是 Anthropic 格式，但提供商是 OpenAI 格式，需要转换
				logger.Info("Converting Anthropic request to OpenAI format")
				requestBody = convertClaudeToOpenAI(requestBody)
				targetURL = provider.ChatEndpointURL()
			} else if !provider.IsOpenAIFormat && isIncomingOpenAIFormat {
				// 请求是 OpenAI 格式，但提供商是 Anthropic 格式，需要转换
				logger.Info("Converting OpenAI request to Anthropic format")
				requestBody = convertOpenAIToClaudeRequest(requestBody)
				targetURL = provider.ChatEndpointURL()
			} else if provider.IsOpenAIFormat && isIncomingOpenAIFormat {
				// 已经是 OpenAI 格式但提供商是 OpenAI 格式
				targetURL = provider.ChatEndpointURL()
			}
			// else: 非 OpenAI 格式，保持原 targetURL

			// 重新序列化请求体
			bodyBytes, _ = json.Marshal(requestBody)
			modifiedRequestBody = string(bodyBytes) // 保存修改后的请求体用于历史记录
		}
	}

	// 创建新请求
	req, err := http.NewRequest(c.Request.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}

	// 复制请求头，但替换 API Key
	for key, values := range c.Request.Header {
		if key == "Authorization" {
			continue // 跳过原始的 Authorization
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 设置正确的 API Key
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// 发送请求，带重试机制
	resp, finalAttempt, err := doRequestWithRetry(req, bodyBytes, provider, 3)
	if err != nil {
		logger.Error("❌ Proxy error after retries: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to proxy request after retries"})
		return
	}
	defer resp.Body.Close()

	retryCount := finalAttempt - 1

	logger.Info("📥 收到目标服务器响应 - Status: %d, Content-Type: %s, Content-Encoding: %s",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"))

	// 复制响应头，但跳过编码相关的头
	// Go 的 http.Client 会自动解压 gzip，所以不应该转发 Content-Encoding
	for key, values := range resp.Header {
		// 跳过这些头，因为内容已经被自动解压或长度会改变
		if key == "Content-Encoding" || key == "Content-Length" {
			continue
		}
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 处理流式响应
	if isStream {
		handleStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, actualModel, userModel, keyID, clientIP, isIncomingOpenAIFormat, retryCount)
		return
	}

	// 处理非流式响应
	handleNonStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, actualModel, userModel, keyID, clientIP, isIncomingOpenAIFormat, retryCount)
}

// handleStreamResponse 处理流式响应（SSE）
func handleStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel, actualModel, userModel string, keyID, clientIP string, isIncomingOpenAIFormat bool, retryCount int) {
	var firstTokenTime time.Time

	c.Status(resp.StatusCode)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logger.Info("Streaming not supported")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// 根据 Content-Encoding 处理解压
	var reader io.Reader = resp.Body
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		if decompressed, err := decompressResponse(resp.Body, contentEncoding); err == nil {
			reader = bytes.NewReader(decompressed)
			logger.Info("Decompressed stream response with %s, decompressed size: %d",
				contentEncoding, len(decompressed))
		} else {
			logger.Error("❌ 解压流式响应失败: %v, Content-Encoding: %s", err, contentEncoding)
		}
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var inputTokens, outputTokens int
	var model string
	firstToken := true
	var responseBody strings.Builder

	for scanner.Scan() {
		line := scanner.Bytes()

		// 如果请求格式与提供商格式不匹配，需要转换响应
		// provider.IsOpenAIFormat == isIncomingOpenAIFormat: 格式匹配，直接转发
		// provider.IsOpenAIFormat && !isIncomingOpenAIFormat: 请求是Anthropic转OpenAI，响应是OpenAI，需转Claude
		// !provider.IsOpenAIFormat && isIncomingOpenAIFormat: 请求是OpenAI转Anthropic，响应是Anthropic，需转OpenAI
		needsConversion := provider.IsOpenAIFormat != isIncomingOpenAIFormat

		if needsConversion && provider.IsOpenAIFormat {
			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					// 转换为 Claude 格式的结束标记
					claudeDone := "data: {\"type\":\"message_stop\"}\n\n"
					if _, err := c.Writer.Write([]byte(claudeDone)); err != nil {
						return
					}
					flusher.Flush()
					responseBody.WriteString(claudeDone)
					continue
				}

				var openaiChunk map[string]interface{}
				if err := json.Unmarshal([]byte(data), &openaiChunk); err == nil {
					// 提取模型信息
					if m, ok := openaiChunk["model"].(string); ok && m != "" {
						model = m
					}

					// 转换为 Claude 格式的流式响应
					claudeChunk := convertOpenAIStreamToClaude(openaiChunk)
					if claudeData, err := json.Marshal(claudeChunk); err == nil {
						claudeLine := "data: " + string(claudeData) + "\n\n"
						if _, err := c.Writer.Write([]byte(claudeLine)); err != nil {
							return
						}
						flusher.Flush()
						responseBody.WriteString(claudeLine)

						// 提取 usage 信息
						if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
							if input, ok := usage["input_tokens"].(int); ok {
								inputTokens = input
							}
							if output, ok := usage["output_tokens"].(int); ok {
								outputTokens = output
							}
						}
					} else {
						logger.Error("❌ Claude chunk JSON 序列化失败: %v, chunk: %+v", err, claudeChunk)
					}
				} else {
					logger.Error("❌ OpenAI chunk JSON 解析失败: %v, 原始数据: %s", err, data)
				}
			} else {
				// 非 data 行直接转发
				if _, err := c.Writer.Write(line); err != nil {
					return
				}
				if _, err := c.Writer.Write([]byte("\n")); err != nil {
					return
				}
				flusher.Flush()
				responseBody.Write(line)
				responseBody.WriteString("\n")
			}
		} else if needsConversion {
			// Anthropic 响应需要转换为 OpenAI 格式 (请求是 OpenAI 格式被转换为 Anthropic)
			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					openaiDone := "data: [DONE]\n\n"
					if _, err := c.Writer.Write([]byte(openaiDone)); err != nil {
						return
					}
					flusher.Flush()
					responseBody.WriteString(openaiDone)
					continue
				}

				var claudeChunk map[string]interface{}
				if err := json.Unmarshal([]byte(data), &claudeChunk); err == nil {
					// 提取模型信息
					if m, ok := claudeChunk["model"].(string); ok && m != "" {
						model = m
					}

					// 转换为 OpenAI 格式的流式响应
					openaiChunk := convertClaudeStreamToOpenAI(claudeChunk)
					if openaiData, err := json.Marshal(openaiChunk); err == nil {
						openaiLine := "data: " + string(openaiData) + "\n\n"
						if _, err := c.Writer.Write([]byte(openaiLine)); err != nil {
							return
						}
						flusher.Flush()
						responseBody.WriteString(openaiLine)

						// 提取 usage 信息
						if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
							if input, ok := usage["input_tokens"].(float64); ok {
								inputTokens = int(input)
							}
							if output, ok := usage["output_tokens"].(float64); ok {
								outputTokens = int(output)
							}
						}
					}
				}
			} else {
				// 非 data 行直接转发
				if _, err := c.Writer.Write(line); err != nil {
					return
				}
				if _, err := c.Writer.Write([]byte("\n")); err != nil {
					return
				}
				flusher.Flush()
				responseBody.Write(line)
				responseBody.WriteString("\n")
			}
		} else {
			// 格式匹配，Claude 格式直接转发
			if _, err := c.Writer.Write(line); err != nil {
				return
			}
			if _, err := c.Writer.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()

			// Collect response body
			responseBody.Write(line)
			responseBody.WriteString("\n")

			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data: ") {
				data := strings.TrimPrefix(lineStr, "data: ")
				if data == "[DONE]" {
					continue
				}

				var streamData map[string]interface{}
				if err := json.Unmarshal([]byte(data), &streamData); err == nil {
					// 提取模型信息
					if m, ok := streamData["model"].(string); ok && m != "" {
						model = m
					}
					// 提取 usage 信息
					if usage, ok := streamData["usage"].(map[string]interface{}); ok {
						if input, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(input)
						}
						if output, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(output)
						}
					}
				}
			}
		}

		if firstToken {
			firstTokenTime = time.Now()
			firstToken = false
		}
	}

	duration := time.Since(startTime).Milliseconds()
	timeToFirst := int64(0)
	if !firstTokenTime.IsZero() {
		timeToFirst = firstTokenTime.Sub(startTime).Milliseconds()
	}

	// 如果响应中没有模型信息，使用实际调用的模型（provider_model）用于 cost 计算
	if model == "" {
		model = actualModel
	}

	// 如果没有获取到模型信息，使用默认值
	if model == "" {
		model = "unknown"
	}

	cost := calculateCost(model, inputTokens, outputTokens)

	// 始终记录统计信息
	stats.RecordUsage(provider.ID, provider.Name, model, "stream", "claude", inputTokens, outputTokens, cost, duration, timeToFirst, keyID, clientIP)

	// Save to history
	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          method,
		Path:            path,
		ClientIP:        clientIP,
		KeyID:           keyID,
		Provider:        provider.Name,
		Model:           model,
		UserModel:       userModel,
		StatusCode:      resp.StatusCode,
		Duration:        duration,
		RequestBody:     requestBody,
		ResponseBody:    responseBody.String(),
		RequestHeaders:  requestHeaders,
		ResponseHeaders: resp.Header,
		RequestSize:     int64(len(requestBody)),
		ResponseSize:    int64(responseBody.Len()),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     inputTokens + outputTokens,
		Cost:            cost,
		RetryCount:      retryCount,
	})
}

// decompressResponse 根据 Content-Encoding 解压响应内容
func decompressResponse(body io.Reader, contentEncoding string) ([]byte, error) {
	switch strings.ToLower(contentEncoding) {
	case "gzip":
		gzReader, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		defer gzReader.Close()
		return io.ReadAll(gzReader)
	case "deflate":
		flateReader := flate.NewReader(body)
		defer flateReader.Close()
		return io.ReadAll(flateReader)
	case "zlib":
		zlibReader, err := zlib.NewReader(body)
		if err != nil {
			return nil, err
		}
		defer zlibReader.Close()
		return io.ReadAll(zlibReader)
	case "br":
		return io.ReadAll(brotli.NewReader(body))
	default:
		// 未知的编码格式或无编码，直接返回原始内容
		return io.ReadAll(body)
	}
}

// handleNonStreamResponse 处理非流式响应
func handleNonStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel, actualModel, userModel string, keyID, clientIP string, isIncomingOpenAIFormat bool, retryCount int) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("❌ 读取响应体失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}

	// 打印响应基本信息
	logger.Info("📥 响应信息 - Status: %d, Content-Type: %s, Content-Encoding: %s, Body大小: %d",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"), len(respBody))

	// 根据 Content-Encoding 尝试解压
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding != "" && contentEncoding != "identity" {
		if decompressed, err := decompressResponse(bytes.NewReader(respBody), contentEncoding); err == nil {
			respBody = decompressed
			logger.Info("✅ 解压成功: %s, 原始大小: %d, 解压后大小: %d",
				contentEncoding, len(respBody), len(decompressed))
		} else {
			logger.Error("❌ 解压失败: %v, Content-Encoding: %s", err, contentEncoding)
		}
	}

	// 打印响应体前200字节用于调试
	respPreview := respBody
	if len(respPreview) > 200 {
		respPreview = respPreview[:200]
	}
	logger.Info("📄 响应体预览: %s", string(respPreview))

	model := actualModel
	var inputTokens, outputTokens int
	var cost float64

	// 解析响应以获取 token 使用情况和模型信息
	var responseBodyForHistory string
	if resp.StatusCode == 200 && len(respBody) > 0 {
		// 尝试解析响应以获取 token 使用情况和模型信息
		var result struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			logger.Error("❌ JSON解析失败: %v, 原始响应: %s", err, string(respBody))
		} else {
			// 如果响应中有模型信息，使用响应中的模型
			if result.Model != "" {
				model = result.Model
			}
			// 保存 token 信息
			inputTokens = result.Usage.InputTokens
			outputTokens = result.Usage.OutputTokens
			logger.Info("✅ JSON解析成功 - Model: %s, InputTokens: %d, OutputTokens: %d",
				model, inputTokens, outputTokens)
		}
		responseBodyForHistory = string(respBody)
	}

	// 如果模型仍然为空，使用默认值
	if model == "" {
		model = "unknown"
	}

	// 记录统计信息
	duration := time.Since(startTime).Milliseconds()
	cost = calculateCost(model, inputTokens, outputTokens)
	stats.RecordUsage(provider.ID, provider.Name, model, "non-stream", "claude",
		inputTokens, outputTokens, cost, duration, 0, keyID, clientIP)

	// Save to history
	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          method,
		Path:            path,
		ClientIP:        clientIP,
		KeyID:           keyID,
		Provider:        provider.Name,
		Model:           model,
		UserModel:       userModel,
		StatusCode:      resp.StatusCode,
		Duration:        duration,
		RequestBody:     requestBody,
		ResponseBody:    responseBodyForHistory,
		RequestHeaders:  requestHeaders,
		ResponseHeaders: resp.Header,
		RequestSize:     int64(len(requestBody)),
		ResponseSize:    int64(len(respBody)),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     inputTokens + outputTokens,
		Cost:            cost,
		RetryCount:      retryCount,
	})

	// 如果请求格式与提供商格式不匹配，需要转换响应格式
	needsConversion := provider.IsOpenAIFormat != isIncomingOpenAIFormat
	if needsConversion && resp.StatusCode == 200 && len(respBody) > 0 {
		if !provider.IsOpenAIFormat && isIncomingOpenAIFormat {
			// 提供商是 Anthropic 格式，请求是 OpenAI 格式，响应是 Anthropic，需要转为 OpenAI
			var claudeResp map[string]interface{}
			if json.Unmarshal(respBody, &claudeResp) == nil {
				openaiResp := convertClaudeToOpenAIResponse(claudeResp)
				if converted, err := json.Marshal(openaiResp); err == nil {
					respBody = converted
					logger.Info("✅ 非流式响应已从 Claude 转换为 OpenAI 格式")
				}
			}
		} else if provider.IsOpenAIFormat && !isIncomingOpenAIFormat {
			// 提供商是 OpenAI 格式，请求是 Anthropic 格式，响应是 OpenAI，需要转为 Anthropic
			var openaiResp map[string]interface{}
			if json.Unmarshal(respBody, &openaiResp) == nil {
				claudeResp := convertOpenAIToClaude(openaiResp)
				if converted, err := json.Marshal(claudeResp); err == nil {
					respBody = converted
					logger.Info("✅ 非流式响应已从 OpenAI 转换为 Claude 格式")
				}
			}
		}
	}

	// 原封不动透传响应体，不做任何格式化处理
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}

func calculateCost(model string, inputTokens, outputTokens int) float64 {
	var inputCost, outputCost float64
	switch {
	case strings.Contains(model, "opus"):
		inputCost, outputCost = 0.000015, 0.000075
	case strings.Contains(model, "sonnet"):
		inputCost, outputCost = 0.000003, 0.000015
	case strings.Contains(model, "haiku"):
		inputCost, outputCost = 0.00000025, 0.00000125
	default:
		inputCost, outputCost = 0.000003, 0.000015
	}
	return float64(inputTokens)*inputCost + float64(outputTokens)*outputCost
}

// convertClaudeToOpenAI 将 Claude 格式的请求转换为 OpenAI 格式
func convertClaudeToOpenAI(claudeReq map[string]interface{}) map[string]interface{} {
	openaiReq := make(map[string]interface{})

	// 复制基本字段
	if model, ok := claudeReq["model"]; ok {
		openaiReq["model"] = model
	}
	if stream, ok := claudeReq["stream"]; ok {
		openaiReq["stream"] = stream
	}
	if temp, ok := claudeReq["temperature"]; ok {
		openaiReq["temperature"] = temp
	}
	if topP, ok := claudeReq["top_p"]; ok {
		openaiReq["top_p"] = topP
	}

	// 转换 max_tokens
	if maxTokens, ok := claudeReq["max_tokens"]; ok {
		openaiReq["max_tokens"] = maxTokens
	}

	// 处理 messages - Claude 的 system 可能是单独字段
	messages := []interface{}{}

	// 如果有 system 字段，添加为第一条消息
	if system, ok := claudeReq["system"].(string); ok && system != "" {
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": system,
		})
	}

	// 添加其他消息
	if claudeMessages, ok := claudeReq["messages"].([]interface{}); ok {
		messages = append(messages, claudeMessages...)
	}

	openaiReq["messages"] = messages

	return openaiReq
}

// convertOpenAIToClaude 将 OpenAI 格式的响应转换为 Claude 格式
func convertOpenAIToClaude(openaiResp map[string]interface{}) map[string]interface{} {
	claudeResp := make(map[string]interface{})

	// 基本字段映射
	if id, ok := openaiResp["id"]; ok {
		claudeResp["id"] = id
	}
	if model, ok := openaiResp["model"]; ok {
		claudeResp["model"] = model
	}

	claudeResp["type"] = "message"
	claudeResp["role"] = "assistant"

	// 转换 choices 为 content
	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					claudeResp["content"] = []map[string]interface{}{
						{
							"type": "text",
							"text": content,
						},
					}
				}
			}
			if finishReason, ok := choice["finish_reason"]; ok {
				claudeResp["stop_reason"] = finishReason
			}
		}
	}

	// 转换 usage
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		claudeUsage := make(map[string]interface{})
		if promptTokens, ok := usage["prompt_tokens"]; ok {
			claudeUsage["input_tokens"] = promptTokens
		}
		if completionTokens, ok := usage["completion_tokens"]; ok {
			claudeUsage["output_tokens"] = completionTokens
		}
		claudeResp["usage"] = claudeUsage
	}

	return claudeResp
}

// convertClaudeToOpenAIResponse 将 Claude 格式的非流式响应转换为 OpenAI 格式
func convertClaudeToOpenAIResponse(claudeResp map[string]interface{}) map[string]interface{} {
	openaiResp := make(map[string]interface{})

	// 基本字段映射
	if id, ok := claudeResp["id"]; ok {
		openaiResp["id"] = id
	}
	if model, ok := claudeResp["model"]; ok {
		openaiResp["model"] = model
	}
	openaiResp["object"] = "chat.completion"
	openaiResp["created"] = time.Now().Unix()

	// 转换 content 为 choices
	var content string
	if contentArr, ok := claudeResp["content"].([]interface{}); ok && len(contentArr) > 0 {
		if firstContent, ok := contentArr[0].(map[string]interface{}); ok {
			if text, ok := firstContent["text"].(string); ok {
				content = text
			}
		}
	}

	var finishReason string
	if stopReason, ok := claudeResp["stop_reason"].(string); ok {
		finishReason = stopReason
	}

	openaiResp["choices"] = []map[string]interface{}{
		{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": finishReason,
		},
	}

	// 转换 usage
	if usage, ok := claudeResp["usage"].(map[string]interface{}); ok {
		openaiUsage := make(map[string]interface{})
		if inputTokens, ok := usage["input_tokens"]; ok {
			openaiUsage["prompt_tokens"] = inputTokens
		}
		if outputTokens, ok := usage["output_tokens"]; ok {
			openaiUsage["completion_tokens"] = outputTokens
		}
		if inputTokens, ok := usage["input_tokens"].(int); ok {
			if outputTokens, ok := usage["output_tokens"].(int); ok {
				openaiUsage["total_tokens"] = inputTokens + outputTokens
			}
		}
		openaiResp["usage"] = openaiUsage
	}

	return openaiResp
}

// convertOpenAIStreamToClaude 将 OpenAI 格式的流式响应块转换为 Claude 格式
func convertOpenAIStreamToClaude(openaiChunk map[string]interface{}) map[string]interface{} {
	claudeChunk := make(map[string]interface{})

	// 基本字段
	if id, ok := openaiChunk["id"]; ok {
		claudeChunk["id"] = id
	}
	if model, ok := openaiChunk["model"]; ok {
		claudeChunk["model"] = model
	}

	// 处理 choices
	if choices, ok := openaiChunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			// 检查是否有 delta
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if content, ok := delta["content"].(string); ok && content != "" {
					// 内容块
					claudeChunk["type"] = "content_block_delta"
					claudeChunk["delta"] = map[string]interface{}{
						"type": "text_delta",
						"text": content,
					}
				} else if role, ok := delta["role"].(string); ok {
					// 开始块
					claudeChunk["type"] = "message_start"
					claudeChunk["message"] = map[string]interface{}{
						"id":    claudeChunk["id"],
						"type":  "message",
						"role":  role,
						"model": claudeChunk["model"],
					}
				}
			}

			// 检查 finish_reason
			if finishReason, ok := choice["finish_reason"]; ok && finishReason != nil {
				claudeChunk["type"] = "message_delta"
				claudeChunk["delta"] = map[string]interface{}{
					"stop_reason": finishReason,
				}
			}
		}
	}

	// 转换 usage（如果有）
	if usage, ok := openaiChunk["usage"].(map[string]interface{}); ok {
		claudeUsage := make(map[string]interface{})
		if promptTokens, ok := usage["prompt_tokens"].(float64); ok {
			claudeUsage["input_tokens"] = int(promptTokens)
		}
		if completionTokens, ok := usage["completion_tokens"].(float64); ok {
			claudeUsage["output_tokens"] = int(completionTokens)
		}
		claudeChunk["usage"] = claudeUsage
	}

	return claudeChunk
}

// convertOpenAIToClaudeRequest 将 OpenAI 格式的请求转换为 Claude 格式
func convertOpenAIToClaudeRequest(openaiReq map[string]interface{}) map[string]interface{} {
	claudeReq := make(map[string]interface{})

	// 复制基本字段
	if model, ok := openaiReq["model"]; ok {
		claudeReq["model"] = model
	}
	if stream, ok := openaiReq["stream"]; ok {
		claudeReq["stream"] = stream
	}
	if temp, ok := openaiReq["temperature"]; ok {
		claudeReq["temperature"] = temp
	}
	if topP, ok := openaiReq["top_p"]; ok {
		claudeReq["top_p"] = topP
	}
	if maxTokens, ok := openaiReq["max_tokens"]; ok {
		claudeReq["max_tokens"] = maxTokens
	}

	// 转换 messages - OpenAI 的 role:system 可能内嵌在 messages 里
	var systemMsg string
	var claudeMessages []interface{}

	if messages, ok := openaiReq["messages"].([]interface{}); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content, _ := msgMap["content"].(string)
				if role == "system" {
					systemMsg = content
				} else {
					claudeMessages = append(claudeMessages, msgMap)
				}
			}
		}
	}

	if systemMsg != "" {
		claudeReq["system"] = systemMsg
	}
	claudeReq["messages"] = claudeMessages

	return claudeReq
}

// convertClaudeStreamToOpenAI 将 Claude 流式响应转换为 OpenAI 格式
func convertClaudeStreamToOpenAI(claudeChunk map[string]interface{}) map[string]interface{} {
	openaiChunk := make(map[string]interface{})

	// 基本字段
	if id, ok := claudeChunk["id"]; ok {
		openaiChunk["id"] = id
	}
	if model, ok := claudeChunk["model"]; ok {
		openaiChunk["model"] = model
	}
	openaiChunk["object"] = "chat.completion.chunk"

	// 处理类型
	chunkType, _ := claudeChunk["type"].(string)

	var choices []interface{}
	switch chunkType {
	case "message_start":
		// 开始消息
		if msg, ok := claudeChunk["message"].(map[string]interface{}); ok {
			choices = []interface{}{
				map[string]interface{}{
					"index": 0,
					"delta": map[string]interface{}{
						"role": msg["role"],
					},
					"finish_reason": nil,
				},
			}
		}
	case "content_block_delta":
		// 内容块增量
		if delta, ok := claudeChunk["delta"].(map[string]interface{}); ok {
			deltaType, _ := delta["type"].(string)
			if deltaType == "text_delta" {
				if text, ok := delta["text"].(string); ok {
					choices = []interface{}{
						map[string]interface{}{
							"index": 0,
							"delta": map[string]interface{}{
								"content": text,
							},
							"finish_reason": nil,
						},
					}
				}
			}
		}
	case "message_delta":
		// 消息结束
		if delta, ok := claudeChunk["delta"].(map[string]interface{}); ok {
			if stopReason, ok := delta["stop_reason"].(string); ok {
				choices = []interface{}{
					map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{},
						"finish_reason": stopReason,
					},
				}
			}
		}
	}

	openaiChunk["choices"] = choices

	// 转换 usage
	if usage, ok := claudeChunk["usage"].(map[string]interface{}); ok {
		openaiUsage := make(map[string]interface{})
		if inputTokens, ok := usage["input_tokens"].(float64); ok {
			openaiUsage["prompt_tokens"] = int(inputTokens)
		}
		if outputTokens, ok := usage["output_tokens"].(float64); ok {
			openaiUsage["completion_tokens"] = int(outputTokens)
		}
		openaiUsage["total_tokens"] = 0
		openaiChunk["usage"] = openaiUsage
	}

	return openaiChunk
}
