package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"io"
	"net/http"
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
	r.Any("/v1/*path", proxyHandler)
}

// doRequestWithRetry 执行请求并在遇到 "try again" 错误时自动重试
func doRequestWithRetry(req *http.Request, bodyBytes []byte, provider *config.Provider, maxRetries int) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
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
			return nil, err
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
			return nil, err
		}

		// 检查响应是否包含 "try again" 错误
		shouldRetry := false
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			var errorResp map[string]interface{}
			if json.Valid(respBody) && json.Unmarshal(respBody, &errorResp) == nil {
				if errorObj, ok := errorResp["error"].(map[string]interface{}); ok {
					if message, ok := errorObj["message"].(string); ok {
						if strings.Contains(strings.ToLower(message), "try again") ||
							strings.Contains(strings.ToLower(message), "high traffic") {
							shouldRetry = true
						}
					}
				}
			}
		}

		// 如果需要重试且还有重试次数
		if shouldRetry && attempt < maxRetries {
			logger.Info("⚠️ 检测到 'try again' 错误 (尝试 %d/%d)，准备重试...", attempt, maxRetries)
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
		return resp, nil
	}

	return lastResp, lastErr
}

func proxyHandler(c *gin.Context) {
	startTime := time.Now()
	requestID := uuid.New().String()

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

	provider := config.GetConfig().GetActiveProvider()
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "No active provider configured",
		})
		return
	}

	clientIP := c.ClientIP()

	// 构建目标 URL
	targetURL := strings.TrimSuffix(provider.BaseURL, "/") + c.Request.URL.Path
	if c.Request.URL.RawQuery != "" {
		targetURL += "?" + c.Request.URL.RawQuery
	}

	// 读取请求体
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	// 解析请求体以检查是否为流式请求，并替换模型参数
	var requestBody map[string]interface{}
	isStream := false
	requestedModel := "unknown"
	modifiedRequestBody := string(bodyBytes) // 用于历史记录的请求体

	if len(bodyBytes) > 0 && json.Valid(bodyBytes) {
		if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
			// 检查是否为流式请求
			if stream, ok := requestBody["stream"].(bool); ok && stream {
				isStream = true
			}

			// 获取请求的模型名称
			if model, ok := requestBody["model"].(string); ok {
				requestedModel = model
				logger.Info("Original request model: %s", model)

				// 使用供应商配置的模型替换请求中的模型
				if provider.Model != "" {
					requestBody["model"] = provider.Model
					requestedModel = provider.Model
					logger.Info("Replaced with provider model: %s", provider.Model)
				}
			}

			// 如果供应商使用 OpenAI 格式，需要将 Claude 格式转换为 OpenAI 格式
			if provider.IsOpenAIFormat {
				requestBody = convertClaudeToOpenAI(requestBody)
				// 智能构建 URL，避免路径重复
				baseURL := strings.TrimSuffix(provider.BaseURL, "/")
				if strings.HasSuffix(baseURL, "/v1") {
					targetURL = baseURL + "/chat/completions"
				} else {
					targetURL = baseURL + "/v1/chat/completions"
				}
			}

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
	resp, err := doRequestWithRetry(req, bodyBytes, provider, 3)
	if err != nil {
		logger.Error("❌ Proxy error after retries: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to proxy request after retries"})
		return
	}
	defer resp.Body.Close()

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
		handleStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, keyID, clientIP)
		return
	}

	// 处理非流式响应
	handleNonStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, keyID, clientIP)
}

// handleStreamResponse 处理流式响应（SSE）
func handleStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string, keyID, clientIP string) {
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

		// 如果是 OpenAI 格式，需要转换为 Claude 格式
		if provider.IsOpenAIFormat {
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
		} else {
			// Claude 格式直接转发
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

	// 如果响应中没有模型信息，使用请求中的模型
	if model == "" {
		model = requestedModel
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
func handleNonStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string, keyID, clientIP string) {
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

	model := requestedModel
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
	})

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
