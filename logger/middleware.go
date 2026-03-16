package logger

import (
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLogger 请求日志中间件
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 记录开始时间
		startTime := time.Now()

		// 处理请求
		c.Next()

		// 计算延迟
		latency := time.Since(startTime)

		// 获取请求信息
		method := c.Request.Method
		path := c.Request.URL.Path
		clientIP := c.ClientIP()
		statusCode := c.Writer.Status()

		// 获取 provider 信息（如果有）
		provider := "unknown"
		if val, exists := c.Get("provider"); exists {
			if p, ok := val.(string); ok {
				provider = p
			}
		}

		// 记录请求日志
		LogRequest(method, path, clientIP, provider, statusCode, latency)

		// 如果是错误状态码，额外记录错误日志
		if statusCode >= 400 {
			errors := c.Errors.String()
			if errors != "" {
				Error("Request error: method=%s path=%s status=%d errors=%s",
					method, path, statusCode, errors)
			}
		}
	}
}
