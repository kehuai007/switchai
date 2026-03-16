package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"switchai/config"
	"switchai/history"
	"switchai/stats"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func RegisterRoutes(r *gin.Engine) {
	// 代理所有 /v1/* 路径
	r.Any("/v1/*path", proxyHandler)
}

func proxyHandler(c *gin.Context) {
	startTime := time.Now()
	requestID := uuid.New().String()

	provider := config.GetConfig().GetActiveProvider()
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "No active provider configured",
		})
		return
	}

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

	// 保存原始请求体用于历史记录
	originalRequestBody := string(bodyBytes)

	// 解析请求体以检查是否为流式请求，并替换模型参数
	var requestBody map[string]interface{}
	isStream := false
	requestedModel := "unknown"
	if len(bodyBytes) > 0 && json.Valid(bodyBytes) {
		if err := json.Unmarshal(bodyBytes, &requestBody); err == nil {
			// 检查是否为流式请求
			if stream, ok := requestBody["stream"].(bool); ok && stream {
				isStream = true
			}

			// 获取请求的模型名称
			if model, ok := requestBody["model"].(string); ok {
				requestedModel = model

				// 如果提供商配置了模型列表，使用第一个模型替换
				if len(provider.Models) > 0 {
					requestBody["model"] = provider.Models[0]
					requestedModel = provider.Models[0]
				}
				log.Printf("Request model: %s", model)
			}

			// 重新序列化请求体
			bodyBytes, _ = json.Marshal(requestBody)
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

	// 发送请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Proxy error: %v", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to proxy request"})
		return
	}
	defer resp.Body.Close()

	// 复制响应头
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	// 处理流式响应
	if isStream {
		handleStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, originalRequestBody, c.Request.Header, requestedModel)
		return
	}

	// 处理非流式响应
	handleNonStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, originalRequestBody, c.Request.Header, requestedModel)
}

// handleStreamResponse 处理流式响应（SSE）
func handleStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string) {
	var firstTokenTime time.Time

	c.Status(resp.StatusCode)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		log.Println("Streaming not supported")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var inputTokens, outputTokens int
	var model string
	firstToken := true
	var responseBody strings.Builder

	for scanner.Scan() {
		line := scanner.Bytes()

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

		if firstToken {
			firstTokenTime = time.Now()
			firstToken = false
		}

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
	stats.RecordUsage(provider.ID, provider.Name, model, "stream", "claude", inputTokens, outputTokens, cost, duration, timeToFirst)

	// Save to history
	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          method,
		Path:            path,
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

// handleNonStreamResponse 处理非流式响应
func handleNonStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel string) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read response"})
		return
	}

	model := requestedModel
	var inputTokens, outputTokens int
	var cost float64

	// 尝试解析响应以获取 token 使用情况和模型信息
	if resp.StatusCode == 200 && len(respBody) > 0 {
		var result struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(respBody, &result); err == nil {
			duration := time.Since(startTime).Milliseconds()

			// 如果响应中有模型信息，使用响应中的模型
			if result.Model != "" {
				model = result.Model
			}

			// 如果模型仍然为空，使用默认值
			if model == "" {
				model = "unknown"
			}

			// 保存 token 信息
			inputTokens = result.Usage.InputTokens
			outputTokens = result.Usage.OutputTokens

			// 记录统计信息（即使 token 为 0 也记录）
			cost = calculateCost(model, inputTokens, outputTokens)
			stats.RecordUsage(provider.ID, provider.Name, model, "non-stream", "claude",
				inputTokens, outputTokens, cost, duration, 0)
		}
	}

	// 格式化响应体 JSON（如果是有效的 JSON）
	formattedRespBody := respBody
	if json.Valid(respBody) {
		var jsonData interface{}
		if err := json.Unmarshal(respBody, &jsonData); err == nil {
			if formatted, err := json.MarshalIndent(jsonData, "", "  "); err == nil {
				formattedRespBody = formatted
			}
		}
	}

	// Save to history
	duration := time.Since(startTime).Milliseconds()
	history.AddRecord(history.RequestRecord{
		ID:              requestID,
		Timestamp:       startTime,
		Method:          method,
		Path:            path,
		Provider:        provider.Name,
		Model:           model,
		StatusCode:      resp.StatusCode,
		Duration:        duration,
		RequestBody:     requestBody,
		ResponseBody:    string(formattedRespBody),
		RequestHeaders:  requestHeaders,
		ResponseHeaders: resp.Header,
		RequestSize:     int64(len(requestBody)),
		ResponseSize:    int64(len(formattedRespBody)),
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     inputTokens + outputTokens,
		Cost:            cost,
	})

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), formattedRespBody)
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
