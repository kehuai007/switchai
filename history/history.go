package history

import (
	"encoding/json"
	"os"
	"switchai/appdata"
	"switchai/logger"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type RequestRecord struct {
	ID              string      `json:"id"`
	Timestamp       time.Time   `json:"timestamp"`
	Method          string      `json:"method"`
	Path            string      `json:"path"`
	ClientIP        string      `json:"client_ip"`
	KeyID           string      `json:"key_id"`      // 使用的服务器密钥ID
	Provider        string      `json:"provider"`
	Model           string      `json:"model"`
	StatusCode      int         `json:"status_code"`
	Duration        int64       `json:"duration_ms"`
	RequestBody     string      `json:"request_body"`
	ResponseBody    string      `json:"response_body"`
	RequestHeaders  interface{} `json:"request_headers"`
	ResponseHeaders interface{} `json:"response_headers"`
	RequestSize     int64       `json:"request_size"`
	ResponseSize    int64       `json:"response_size"`
	InputTokens     int         `json:"input_tokens"`
	OutputTokens    int         `json:"output_tokens"`
	TotalTokens     int         `json:"total_tokens"`
	Cost            float64     `json:"cost"`
}

type History struct {
	Records     []RequestRecord       `json:"records"`
	mu          sync.RWMutex
	dirty       bool                  // 标记数据是否变动
	quitChan    chan struct{}         // 用于退出后台 goroutine
	clients     map[*websocket.Conn]bool
	broadcast   chan RequestRecord    // 广播新记录
}

var history *History

func Init() error {
	history = &History{
		Records:   []RequestRecord{},
		quitChan:  make(chan struct{}),
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan RequestRecord, 100),
	}

	// Load from file if exists
	if err := history.loadFromFile(); err != nil {
		// If file doesn't exist, just start fresh
		return nil
	}

	// 启动后台定时保存 goroutine
	go history.backgroundSave()

	// 启动广播协程
	go history.handleBroadcast()

	return nil
}

// backgroundSave 后台定时保存数据
func (h *History) backgroundSave() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-h.quitChan:
			// 确保退出前保存一次
			h.saveToFile()
			return
		case <-ticker.C:
			// 检查是否有变动
			h.mu.Lock()
			if h.dirty {
				h.dirty = false
				h.mu.Unlock()
				h.saveToFile()
			} else {
				h.mu.Unlock()
			}
		}
	}
}

// Shutdown 停止后台保存并保存数据
func Shutdown() {
	if history == nil {
		return
	}
	close(history.quitChan)
}

// AddClient 添加 WebSocket 客户端
func AddClient(conn *websocket.Conn) {
	history.mu.Lock()
	defer history.mu.Unlock()
	history.clients[conn] = true
}

// RemoveClient 移除 WebSocket 客户端
func RemoveClient(conn *websocket.Conn) {
	history.mu.Lock()
	defer history.mu.Unlock()
	delete(history.clients, conn)
	conn.Close()
}

// handleBroadcast 广播新记录到所有客户端
func (h *History) handleBroadcast() {
	for record := range h.broadcast {
		h.mu.RLock()
		total := len(h.Records)
		// 发送单条记录（包含总数）
		msg := gin.H{
			"id":       record.ID,
			"total":    total,
			"timestamp": record.Timestamp,
			"method":   record.Method,
			"path":     record.Path,
			"client_ip": record.ClientIP,
			"key_id":   record.KeyID,
			"provider":  record.Provider,
			"model":    record.Model,
			"status_code": record.StatusCode,
			"duration_ms": record.Duration,
			"input_tokens": record.InputTokens,
			"output_tokens": record.OutputTokens,
			"total_tokens": record.TotalTokens,
			"cost":     record.Cost,
		}
		for client := range h.clients {
			err := client.WriteJSON(msg)
			if err != nil {
				logger.Error("WebSocket write error: %v", err)
				client.Close()
				delete(h.clients, client)
			}
		}
		h.mu.RUnlock()
	}
}

func (h *History) loadFromFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	data, err := os.ReadFile(appdata.GetConfigPath("history.json"))
	if err != nil {
		return err
	}

	return json.Unmarshal(data, &h.Records)
}

func (h *History) saveToFile() error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	data, err := json.Marshal(h.Records)
	if err != nil {
		return err
	}

	return os.WriteFile(appdata.GetConfigPath("history.json"), data, 0644)
}

func AddRecord(record RequestRecord) {
	history.mu.Lock()

	history.Records = append(history.Records, record)

	// Keep only last 1000 records
	if len(history.Records) > 1000 {
		history.Records = history.Records[len(history.Records)-1000:]
	}

	// 标记数据已变动
	history.dirty = true

	history.mu.Unlock()

	// 广播到所有 WebSocket 客户端
	select {
	case history.broadcast <- record:
	default:
		logger.Info("History broadcast channel full, skipping")
	}

	// 打印历史记录日志
	logger.Info("[%s] %s %s | %s | %s | %d | %dms | in:%d out:%d",
		record.Method, record.Path, record.ClientIP, record.Provider, record.Model,
		record.StatusCode, record.Duration, record.InputTokens, record.OutputTokens)
}

func GetRecords(page, pageSize int) ([]RequestRecord, int) {
	history.mu.RLock()
	defer history.mu.RUnlock()

	total := len(history.Records)

	// Calculate pagination
	start := (page - 1) * pageSize
	if start >= total {
		return []RequestRecord{}, total
	}

	end := start + pageSize
	if end > total {
		end = total
	}

	// Return in reverse order (newest first)
	result := make([]RequestRecord, 0, end-start)
	for i := total - 1 - start; i >= total-end; i-- {
		if i >= 0 && i < total {
			result = append(result, history.Records[i])
		}
	}

	return result, total
}

func GetRecord(id string) *RequestRecord {
	history.mu.RLock()
	defer history.mu.RUnlock()

	for i := range history.Records {
		if history.Records[i].ID == id {
			return &history.Records[i]
		}
	}

	return nil
}
