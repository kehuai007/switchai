package history

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type RequestRecord struct {
	ID              string      `json:"id"`
	Timestamp       time.Time   `json:"timestamp"`
	Method          string      `json:"method"`
	Path            string      `json:"path"`
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
	Records []RequestRecord `json:"records"`
	mu      sync.RWMutex
}

var history *History

func Init() error {
	history = &History{
		Records: []RequestRecord{},
	}

	// Load from file if exists
	if err := history.loadFromFile(); err != nil {
		// If file doesn't exist, just start fresh
		return nil
	}

	return nil
}

func (h *History) loadFromFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	data, err := os.ReadFile("history.json")
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

	return os.WriteFile("history.json", data, 0644)
}

func AddRecord(record RequestRecord) {
	history.mu.Lock()

	history.Records = append(history.Records, record)

	// Keep only last 1000 records
	if len(history.Records) > 1000 {
		history.Records = history.Records[len(history.Records)-1000:]
	}

	history.mu.Unlock()

	// Save to file asynchronously
	go history.saveToFile()
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
