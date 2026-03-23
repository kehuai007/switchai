package stats

import (
	"encoding/json"
	"log"
	"os"
	"switchai/appdata"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type UsageRecord struct {
	ProviderID   string    `json:"provider_id"`
	ProviderName string    `json:"provider_name"`
	Model        string    `json:"model"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	TotalTokens  int       `json:"total_tokens"`
	Cost         float64   `json:"cost"`
	Duration     int64     `json:"duration_ms"`
	TimeToFirst  int64     `json:"time_to_first_ms"`
	Timestamp    time.Time `json:"timestamp"`
	Group        string    `json:"group"`
	Type         string    `json:"type"`
}

type ProviderStats struct {
	ProviderID   string  `json:"provider_id"`
	ProviderName string  `json:"provider_name"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	TotalCost    float64 `json:"total_cost"`
	RequestCount int     `json:"request_count"`
}

type Stats struct {
	Records       []UsageRecord             `json:"records"`
	ProviderStats map[string]*ProviderStats `json:"provider_stats"`
	mu            sync.RWMutex
	clients       map[*websocket.Conn]bool
	broadcast     chan UsageRecord
	dirty         bool // 标记数据是否需要保存
}

type PersistentStats struct {
	Records       []UsageRecord             `json:"records"`
	ProviderStats map[string]*ProviderStats `json:"provider_stats"`
}

var stats *Stats

func Init() {
	stats = &Stats{
		Records:       []UsageRecord{},
		ProviderStats: make(map[string]*ProviderStats),
		clients:       make(map[*websocket.Conn]bool),
		broadcast:     make(chan UsageRecord, 100),
		dirty:         false,
	}

	// 从文件加载统计数据
	if err := stats.loadFromFile(); err != nil {
		log.Printf("⚠️ 加载统计数据失败: %v，使用空数据", err)
	} else {
		log.Println("✅ 统计数据已从文件加载")
	}

	// 启动广播协程
	go stats.handleBroadcast()

	// 启动定时保存协程
	go stats.autoSave()
}

func (s *Stats) handleBroadcast() {
	for record := range s.broadcast {
		s.mu.RLock()
		for client := range s.clients {
			err := client.WriteJSON(record)
			if err != nil {
				log.Printf("WebSocket write error: %v", err)
				client.Close()
				delete(s.clients, client)
			}
		}
		s.mu.RUnlock()
	}
}

func (s *Stats) AddClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = true
}

func (s *Stats) RemoveClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	conn.Close()
}

func (s *Stats) loadFromFile() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(appdata.GetConfigPath("stats/stats.json"))
	if err != nil {
		return err
	}

	var persistent PersistentStats
	if err := json.Unmarshal(data, &persistent); err != nil {
		return err
	}

	s.Records = persistent.Records
	s.ProviderStats = persistent.ProviderStats

	return nil
}

func (s *Stats) saveToFile() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	persistent := PersistentStats{
		Records:       s.Records,
		ProviderStats: s.ProviderStats,
	}

	data, err := json.MarshalIndent(persistent, "", "  ")
	if err != nil {
		return err
	}

	// 确保目录存在
	statsDir := appdata.GetDataDir() + "/stats"
	os.MkdirAll(statsDir, 0755)

	return os.WriteFile(appdata.GetConfigPath("stats/stats.json"), data, 0644)
}

// autoSave 定时保存数据
func (s *Stats) autoSave() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		if s.dirty {
			s.mu.Unlock()
			if err := s.saveToFile(); err != nil {
				log.Printf("⚠️ 自动保存统计数据失败: %v", err)
			} else {
				s.mu.Lock()
				s.dirty = false
				s.mu.Unlock()
			}
		} else {
			s.mu.Unlock()
		}
	}
}

// Shutdown 立即保存数据（用于程序退出时）
func Shutdown() {
	if stats == nil {
		return
	}

	stats.mu.Lock()
	needSave := stats.dirty
	stats.mu.Unlock()

	if needSave {
		if err := stats.saveToFile(); err != nil {
			log.Printf("⚠️ 保存统计数据失败: %v", err)
		} else {
			log.Println("✅ 统计数据已保存")
		}
	}
}

func RecordUsage(providerID, providerName, model, group, reqType string, inputTokens, outputTokens int, cost float64, duration, timeToFirst int64) {
	stats.mu.Lock()

	record := UsageRecord{
		ProviderID:   providerID,
		ProviderName: providerName,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
		Cost:         cost,
		Duration:     duration,
		TimeToFirst:  timeToFirst,
		Timestamp:    time.Now(),
		Group:        group,
		Type:         reqType,
	}

	stats.Records = append(stats.Records, record)

	// 只保留最近 1000 条记录
	if len(stats.Records) > 1000 {
		stats.Records = stats.Records[len(stats.Records)-1000:]
	}

	// 更新供应商统计
	if _, exists := stats.ProviderStats[providerID]; !exists {
		stats.ProviderStats[providerID] = &ProviderStats{
			ProviderID:   providerID,
			ProviderName: providerName,
		}
	}
	providerStat := stats.ProviderStats[providerID]
	providerStat.ProviderName = providerName // 每次都更新名字，确保同步
	providerStat.InputTokens += inputTokens
	providerStat.OutputTokens += outputTokens
	providerStat.TotalTokens += inputTokens + outputTokens
	providerStat.TotalCost += cost
	providerStat.RequestCount++

	stats.dirty = true // 标记需要保存
	stats.mu.Unlock()

	// 打印日志
	log.Printf("📊 Token统计 | 时间: %s | 令牌: %d输入/%d输出 | 分组: %s | 类型: %s | 模型: %s | 用时: %dms | 首字: %dms | 花费: $%.6f",
		record.Timestamp.Format("15:04:05"),
		inputTokens,
		outputTokens,
		group,
		reqType,
		model,
		duration,
		timeToFirst,
		cost,
	)

	// 广播到所有WebSocket客户端
	select {
	case stats.broadcast <- record:
	default:
		log.Println("Broadcast channel full, skipping")
	}
}

func GetStats() *Stats {
	return stats
}

func (s *Stats) GetSummary() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totalInput := 0
	totalOutput := 0
	totalCost := 0.0

	for _, providerStat := range s.ProviderStats {
		totalInput += providerStat.InputTokens
		totalOutput += providerStat.OutputTokens
		totalCost += providerStat.TotalCost
	}

	// 转换为数组格式
	providerStatsArray := make([]*ProviderStats, 0, len(s.ProviderStats))
	for _, stat := range s.ProviderStats {
		providerStatsArray = append(providerStatsArray, stat)
	}

	return map[string]interface{}{
		"total_input_tokens":  totalInput,
		"total_output_tokens": totalOutput,
		"total_tokens":        totalInput + totalOutput,
		"total_cost":          totalCost,
		"provider_stats":      providerStatsArray,
		"recent_records":      s.Records[max(0, len(s.Records)-10):],
	}
}

// ResetStats 重置所有统计数据
func ResetStats() {
	stats.mu.Lock()
	stats.Records = []UsageRecord{}
	stats.ProviderStats = make(map[string]*ProviderStats)
	stats.dirty = true
	stats.mu.Unlock()

	log.Println("✅ 所有统计数据已重置")

	// 立即保存到文件
	if err := stats.saveToFile(); err != nil {
		log.Printf("⚠️ 保存统计数据失败: %v", err)
	}
}

// ResetProviderStats 重置指定供应商的统计数据
func ResetProviderStats(providerID string) {
	stats.mu.Lock()

	// 删除该供应商的统计
	delete(stats.ProviderStats, providerID)

	// 删除该供应商的记录
	newRecords := make([]UsageRecord, 0)
	for _, record := range stats.Records {
		if record.ProviderID != providerID {
			newRecords = append(newRecords, record)
		}
	}
	stats.Records = newRecords
	stats.dirty = true
	stats.mu.Unlock()

	log.Printf("✅ 供应商 %s 的统计数据已重置", providerID)

	// 立即保存到文件
	if err := stats.saveToFile(); err != nil {
		log.Printf("⚠️ 保存统计数据失败: %v", err)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
