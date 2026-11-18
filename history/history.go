package history

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"switchai/appdata"
	"switchai/logger"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

type RequestRecord struct {
	ID               string      `json:"id"`
	Timestamp        time.Time   `json:"timestamp"`
	Method           string      `json:"method"`
	Path             string      `json:"path"`
	ClientIP         string      `json:"client_ip"`
	KeyID            string      `json:"key_id"`
	Provider         string      `json:"provider"`
	Model            string      `json:"model"`
	UserModel        string      `json:"user_model"`
	StatusCode       int         `json:"status_code"`
	Duration         int64       `json:"duration_ms"`
	RequestBody      string      `json:"request_body"`
	ResponseBody     string      `json:"response_body"`
	RequestHeaders   interface{} `json:"request_headers"`
	ResponseHeaders  interface{} `json:"response_headers"`
	RequestSize      int64       `json:"request_size"`
	ResponseSize     int64       `json:"response_size"`
	InputTokens      int         `json:"input_tokens"`
	OutputTokens     int         `json:"output_tokens"`
	CacheReadTokens  int         `json:"cache_read_tokens"`
	CacheWriteTokens int         `json:"cache_write_tokens"`
	TotalTokens      int         `json:"total_tokens"`
	RetryCount       int         `json:"retry_count"`
}

// RecordFilters 是请求记录的筛选条件；空值表示不限制。
type RecordFilters struct {
	RequestID string
	UserModel string
	Model     string
	Provider  string
	KeyID     string
	ClientIP  string
}

func (f RecordFilters) empty() bool {
	return f.RequestID == "" && f.UserModel == "" && f.Model == "" &&
		f.Provider == "" && f.KeyID == "" && f.ClientIP == ""
}

func (f RecordFilters) whereClause() (string, []interface{}) {
	var conds []string
	var args []interface{}
	// request id 支持前缀匹配：表格展示的是去掉横线的前 8 位，用户可能直接粘贴。
	if f.RequestID != "" {
		conds = append(conds, "id LIKE ?")
		args = append(args, f.RequestID+"%")
	}
	add := func(column, value string) {
		if value != "" {
			conds = append(conds, column+" = ?")
			args = append(args, value)
		}
	}
	add("user_model", f.UserModel)
	add("model", f.Model)
	add("provider", f.Provider)
	add("key_id", f.KeyID)
	add("client_ip", f.ClientIP)
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// FilterOptions 是历史记录页下拉筛选的可选值。
type FilterOptions struct {
	UserModels []string `json:"user_models"`
	Models     []string `json:"models"`
	Providers  []string `json:"providers"`
	KeyIDs     []string `json:"key_ids"`
	ClientIPs  []string `json:"client_ips"`
}

var (
	db        *sql.DB
	history   *History
	broadcast chan RequestRecord
	clients   map[*websocket.Conn]bool
	clientsMu sync.RWMutex

	// 首页缓存
	homeCache      []RequestRecord
	homeCacheMu    sync.RWMutex
	homeCacheSize  = 20 // 首页缓存大小
	homeCacheTotal int  // 缓存时的总记录数
)

type History struct {
	quitChan chan struct{}
}

func Init() error {
	// Ensure data directory exists
	dataDir := appdata.GetConfigPath("")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	dbPath := filepath.Join(dataDir, "history.db")
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// Create table and index
	if err := initDB(); err != nil {
		db.Close()
		return err
	}

	// 初始化筛选项字典。首次升级时从历史表回填一次，之后由新记录增量维护。
	populateFilterValues()

	history = &History{
		quitChan: make(chan struct{}),
	}
	broadcast = make(chan RequestRecord, 1000)
	clients = make(map[*websocket.Conn]bool)

	// 加载首页缓存
	loadHomeCache()

	// Start broadcast goroutine
	go history.handleBroadcast()

	return nil
}

// loadHomeCache 加载首页缓存
func loadHomeCache() {
	homeCacheMu.Lock()
	defer homeCacheMu.Unlock()

	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, retry_count
		FROM history ORDER BY timestamp DESC LIMIT ?`, homeCacheSize)
	if err != nil {
		logger.Error("Failed to load home cache: %v", err)
		return
	}
	defer rows.Close()

	homeCache = nil
	for rows.Next() {
		var r RequestRecord
		var timestamp int64
		var reqHeaders, respHeaders sql.NullString
		var method, path, clientIP, keyID, provider, model, userModel sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens sql.NullInt64
		var cacheReadTokens, cacheWriteTokens, totalTokens sql.NullInt64
		var requestBody, responseBody sql.NullString
		var requestSize, responseSize sql.NullInt64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
			&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
			&requestSize, &responseSize, &inputTokens, &outputTokens,
			&cacheReadTokens, &cacheWriteTokens, &totalTokens, &r.RetryCount)
		if err != nil {
			logger.Error("Failed to scan cache record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.UserModel = userModel.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestBody = requestBody.String
		r.ResponseBody = responseBody.String
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.CacheReadTokens = int(cacheReadTokens.Int64)
		r.CacheWriteTokens = int(cacheWriteTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)

		if reqHeaders.Valid {
			json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
		}
		if respHeaders.Valid {
			json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
		}

		homeCache = append(homeCache, r)
	}

	// 获取缓存时的总记录数
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&homeCacheTotal)
	logger.Info("Home cache loaded: %d records, total: %d", len(homeCache), homeCacheTotal)
}

func initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS history (
		id TEXT PRIMARY KEY,
		timestamp INTEGER NOT NULL,
		method TEXT,
		path TEXT,
		client_ip TEXT,
		key_id TEXT,
		provider TEXT,
		model TEXT,
		user_model TEXT DEFAULT '',
		status_code INTEGER,
		duration_ms INTEGER,
		request_body TEXT,
		response_body TEXT,
		request_headers TEXT,
		response_headers TEXT,
		request_size INTEGER,
		response_size INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER,
		cost REAL,
		retry_count INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_history_timestamp ON history(timestamp DESC);

	CREATE TABLE IF NOT EXISTS history_filter_values (
		kind TEXT NOT NULL,
		value TEXT NOT NULL,
		PRIMARY KEY (kind, value)
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Migration: 老库补列。重复执行会报 duplicate column，故意忽略。
	db.Exec("ALTER TABLE history ADD COLUMN user_model TEXT DEFAULT ''")
	db.Exec("ALTER TABLE history ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE history ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE history ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0")

	return nil
}

func filterPairs(record RequestRecord) [][2]string {
	return [][2]string{
		{"user_model", record.UserModel},
		{"model", record.Model},
		{"provider", record.Provider},
		{"key_id", record.KeyID},
		{"client_ip", record.ClientIP},
	}
}

func recordFilterValues(record RequestRecord) {
	for _, pair := range filterPairs(record) {
		if pair[1] == "" {
			continue
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO history_filter_values (kind, value) VALUES (?, ?)`,
			pair[0], pair[1]); err != nil {
			logger.Error("Failed to record filter value %s=%s: %v", pair[0], pair[1], err)
		}
	}
}

// populateFilterValues 只在筛选项字典为空时执行一次全表回填，
// 避免每次打开日志页都对 history 大表做 DISTINCT 扫描。
func populateFilterValues() {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM history_filter_values").Scan(&count); err != nil {
		logger.Error("Failed to count filter values: %v", err)
		return
	}
	if count > 0 {
		return
	}

	rows, err := db.Query(`SELECT user_model, model, provider, key_id, client_ip FROM history`)
	if err != nil {
		logger.Error("Failed to load filter values: %v", err)
		return
	}
	defer rows.Close()

	tx, err := db.Begin()
	if err != nil {
		logger.Error("Failed to begin filter values tx: %v", err)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO history_filter_values (kind, value) VALUES (?, ?)`)
	if err != nil {
		logger.Error("Failed to prepare filter values insert: %v", err)
		return
	}
	defer stmt.Close()

	for rows.Next() {
		var userModel, model, provider, keyID, clientIP sql.NullString
		if err := rows.Scan(&userModel, &model, &provider, &keyID, &clientIP); err != nil {
			continue
		}
		values := [][2]string{
			{"user_model", userModel.String},
			{"model", model.String},
			{"provider", provider.String},
			{"key_id", keyID.String},
			{"client_ip", clientIP.String},
		}
		for _, pair := range values {
			if pair[1] != "" {
				stmt.Exec(pair[0], pair[1])
			}
		}
	}
	if err := tx.Commit(); err != nil {
		logger.Error("Failed to commit filter values: %v", err)
	}
}

// GetFilterOptions 返回各筛选维度的可选值。
func GetFilterOptions() FilterOptions {
	load := func(kind string) []string {
		values := make([]string, 0)
		rows, err := db.Query(`SELECT value FROM history_filter_values WHERE kind = ? ORDER BY value LIMIT 500`, kind)
		if err != nil {
			logger.Error("Failed to load filter options for %s: %v", kind, err)
			return values
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err == nil {
				values = append(values, v)
			}
		}
		return values
	}

	return FilterOptions{
		UserModels: load("user_model"),
		Models:     load("model"),
		Providers:  load("provider"),
		KeyIDs:     load("key_id"),
		ClientIPs:  load("client_ip"),
	}
}

// cleanupOldRecords 删除超过1000条的旧数据
func cleanupOldRecords() {
	const maxRecords = 1000

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if err != nil {
		logger.Error("Failed to count history records: %v", err)
		return
	}

	if count > maxRecords {
		// 计算需要删除的数量
		deleteCount := count - maxRecords
		// 删除最老的记录（按时间戳升序，删除最早的）
		_, err := db.Exec(`
			DELETE FROM history WHERE id IN (
				SELECT id FROM history ORDER BY timestamp ASC LIMIT ?
			)`, deleteCount)
		if err != nil {
			logger.Error("Failed to cleanup old history records: %v", err)
		} else {
			logger.Info("Cleaned up %d old history records", deleteCount)
		}
	}
}

func Shutdown() {
	if history == nil {
		return
	}
	close(history.quitChan)
	if db != nil {
		db.Close()
	}
}

func AddClient(conn *websocket.Conn) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clients[conn] = true
}

func RemoveClient(conn *websocket.Conn) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	conn.Close()
	delete(clients, conn)
}

func (h *History) handleBroadcast() {
	for record := range broadcast {
		clientsMu.RLock()
		msg := buildBroadcastMessage(record)
		for client := range clients {
			err := client.WriteJSON(msg)
			if err != nil {
				logger.Error("WebSocket write error: %v", err)
				client.Close()
				delete(clients, client)
			}
		}
		clientsMu.RUnlock()
	}
}

// buildBroadcastMessage 构造 WebSocket 推送给客户端的 JSON。
// 提取出来便于单测：log.html 的 prependRecord 依赖这些字段名，回归时单测能直接守住。
func buildBroadcastMessage(record RequestRecord) gin.H {
	return gin.H{
		"id":                 record.ID,
		"timestamp":          record.Timestamp,
		"method":             record.Method,
		"path":               record.Path,
		"client_ip":          record.ClientIP,
		"key_id":             record.KeyID,
		"provider":           record.Provider,
		"model":              record.Model,
		"user_model":         record.UserModel,
		"status_code":        record.StatusCode,
		"duration_ms":        record.Duration,
		"input_tokens":       record.InputTokens,
		"output_tokens":      record.OutputTokens,
		"cache_read_tokens":  record.CacheReadTokens,
		"cache_write_tokens": record.CacheWriteTokens,
		"total_tokens":       record.TotalTokens,
		"retry_count":        record.RetryCount,
	}
}

// BroadcastRecord 从外部包广播记录到历史 WebSocket 客户端
func BroadcastRecord(providerID, providerName, model, userModel string, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int, duration int64, timestamp time.Time, keyID, clientIP string) {
	record := RequestRecord{
		ID:               "",
		Timestamp:        timestamp,
		Method:           "",
		Path:             "",
		ClientIP:         clientIP,
		KeyID:            keyID,
		Provider:         providerName,
		Model:            model,
		UserModel:        userModel,
		StatusCode:       0,
		Duration:         duration,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		CacheReadTokens:  cacheReadTokens,
		CacheWriteTokens: cacheWriteTokens,
		TotalTokens:      inputTokens + outputTokens + cacheReadTokens + cacheWriteTokens,
	}
	broadcast <- record
}

func AddRecord(record RequestRecord) {
	// Insert into SQLite
	var reqHeaders, respHeaders string
	if record.RequestHeaders != nil {
		b, _ := json.Marshal(record.RequestHeaders)
		reqHeaders = string(b)
	}
	if record.ResponseHeaders != nil {
		b, _ := json.Marshal(record.ResponseHeaders)
		respHeaders = string(b)
	}

	_, err := db.Exec(`
		INSERT INTO history (id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, retry_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Timestamp.UnixNano(), record.Method, record.Path, record.ClientIP,
		record.KeyID, record.Provider, record.Model, record.UserModel, record.StatusCode, record.Duration,
		record.RequestBody, record.ResponseBody, reqHeaders, respHeaders,
		record.RequestSize, record.ResponseSize, record.InputTokens, record.OutputTokens,
		record.CacheReadTokens, record.CacheWriteTokens, record.TotalTokens, record.RetryCount)
	if err != nil {
		logger.Error("Failed to insert history record: %v", err)
	}

	recordFilterValues(record)

	// Cleanup old records if exceeds 1000
	cleanupOldRecords()

	// 更新首页缓存：头部插入新记录
	updateHomeCache(record)

	// Broadcast to WebSocket clients
	go func(r RequestRecord) {
		broadcast <- r
	}(record)

	logger.Info("[%s] %s %s | %s | %s | %d | %dms | in:%d out:%d cacheR:%d cacheW:%d",
		record.Method, record.Path, record.ClientIP, record.Provider, record.Model,
		record.StatusCode, record.Duration, record.InputTokens, record.OutputTokens,
		record.CacheReadTokens, record.CacheWriteTokens)
}

// updateHomeCache 更新首页缓存，头部插入新记录
func updateHomeCache(record RequestRecord) {
	homeCacheMu.Lock()
	defer homeCacheMu.Unlock()

	// 头部插入新记录
	newCache := make([]RequestRecord, 0, homeCacheSize)
	newCache = append(newCache, record)
	// 追加原有缓存（保留前 homeCacheSize-1 条）
	for i := 0; i < len(homeCache) && len(newCache) < homeCacheSize; i++ {
		newCache = append(newCache, homeCache[i])
	}
	homeCache = newCache
	homeCacheTotal++
}

// getHomeCache 返回缓存的副本
func getHomeCache() ([]RequestRecord, int) {
	homeCacheMu.RLock()
	defer homeCacheMu.RUnlock()

	if homeCache == nil {
		return []RequestRecord{}, 0
	}
	// 返回副本避免并发问题
	cacheCopy := make([]RequestRecord, len(homeCache))
	copy(cacheCopy, homeCache)
	return cacheCopy, homeCacheTotal
}

// RecordSummary 列表展示用的精简记录
type RecordSummary struct {
	ID               string    `json:"id"`
	Timestamp        time.Time `json:"timestamp"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	ClientIP         string    `json:"client_ip"`
	KeyID            string    `json:"key_id"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	UserModel        string    `json:"user_model"`
	StatusCode       int       `json:"status_code"`
	Duration         int64     `json:"duration_ms"`
	RequestSize      int64     `json:"request_size"`
	ResponseSize     int64     `json:"response_size"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	RetryCount       int       `json:"retry_count"`
}

func GetRecords(page, pageSize int) ([]RequestRecord, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	// Get total count
	var total int
	err := db.QueryRow("SELECT COUNT(*) FROM history").Scan(&total)
	if err != nil {
		logger.Error("Failed to count records: %v", err)
		return []RequestRecord{}, 0
	}

	offset := (page - 1) * pageSize
	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, retry_count
		FROM history ORDER BY timestamp DESC LIMIT ? OFFSET ?`, pageSize, offset)
	if err != nil {
		logger.Error("Failed to query records: %v", err)
		return []RequestRecord{}, 0
	}
	defer rows.Close()

	var records []RequestRecord
	for rows.Next() {
		var r RequestRecord
		var timestamp int64
		var reqHeaders, respHeaders sql.NullString
		var method, path, clientIP, keyID, provider, model, userModel sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens sql.NullInt64
		var cacheReadTokens, cacheWriteTokens, totalTokens sql.NullInt64
		var requestBody, responseBody sql.NullString
		var requestSize, responseSize sql.NullInt64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
			&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
			&requestSize, &responseSize, &inputTokens, &outputTokens,
			&cacheReadTokens, &cacheWriteTokens, &totalTokens, &r.RetryCount)
		if err != nil {
			logger.Error("Failed to scan record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.UserModel = userModel.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestBody = requestBody.String
		r.ResponseBody = responseBody.String
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.CacheReadTokens = int(cacheReadTokens.Int64)
		r.CacheWriteTokens = int(cacheWriteTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)

		if reqHeaders.Valid {
			json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
		}
		if respHeaders.Valid {
			json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
		}

		records = append(records, r)
	}

	return records, total
}

// GetRecordsSummary 获取精简的记录列表（不含 request_body、response_body 等大字段）。
// filters 非空时直接查库并做 COUNT；无筛选且第一页时走内存首页缓存。
func GetRecordsSummary(page, pageSize int, filters RecordFilters) ([]RecordSummary, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	// 第1页、无筛选且请求大小不超过缓存大小时，使用缓存
	if filters.empty() && page == 1 && pageSize <= homeCacheSize {
		homeCacheMu.RLock()
		defer homeCacheMu.RUnlock()

		summaries := make([]RecordSummary, 0, len(homeCache))
		for _, r := range homeCache {
			summaries = append(summaries, RecordSummary{
				ID:               r.ID,
				Timestamp:        r.Timestamp,
				Method:           r.Method,
				Path:             r.Path,
				ClientIP:         r.ClientIP,
				KeyID:            r.KeyID,
				Provider:         r.Provider,
				Model:            r.Model,
				UserModel:        r.UserModel,
				StatusCode:       r.StatusCode,
				Duration:         r.Duration,
				RequestSize:      r.RequestSize,
				ResponseSize:     r.ResponseSize,
				InputTokens:      r.InputTokens,
				OutputTokens:     r.OutputTokens,
				CacheReadTokens:  r.CacheReadTokens,
				CacheWriteTokens: r.CacheWriteTokens,
				TotalTokens:      r.TotalTokens,
				RetryCount:       r.RetryCount,
			})
		}
		return summaries, homeCacheTotal
	}

	where, filterArgs := filters.whereClause()

	// Get total count
	var total int
	err := db.QueryRow("SELECT COUNT(*) FROM history"+where, filterArgs...).Scan(&total)
	if err != nil {
		logger.Error("Failed to count records: %v", err)
		return []RecordSummary{}, 0
	}

	offset := (page - 1) * pageSize
	queryArgs := make([]interface{}, 0, len(filterArgs)+2)
	queryArgs = append(queryArgs, filterArgs...)
	queryArgs = append(queryArgs, pageSize, offset)
	rows, err := db.Query(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
			status_code, duration_ms, request_size, response_size,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, retry_count
		FROM history`+where+` ORDER BY timestamp DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		logger.Error("Failed to query records: %v", err)
		return []RecordSummary{}, 0
	}
	defer rows.Close()

	var records []RecordSummary
	for rows.Next() {
		var r RecordSummary
		var timestamp int64
		var method, path, clientIP, keyID, provider, model, userModel sql.NullString
		var statusCode, durationMs, inputTokens, outputTokens, retryCount sql.NullInt64
		var cacheReadTokens, cacheWriteTokens, totalTokens sql.NullInt64
		var requestSize, responseSize sql.NullInt64

		err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
			&statusCode, &durationMs, &requestSize, &responseSize,
			&inputTokens, &outputTokens, &cacheReadTokens, &cacheWriteTokens, &totalTokens, &retryCount)
		if err != nil {
			logger.Error("Failed to scan record: %v", err)
			continue
		}

		r.Timestamp = time.Unix(0, timestamp)
		r.Method = method.String
		r.Path = path.String
		r.ClientIP = clientIP.String
		r.KeyID = keyID.String
		r.Provider = provider.String
		r.Model = model.String
		r.UserModel = userModel.String
		r.StatusCode = int(statusCode.Int64)
		r.Duration = durationMs.Int64
		r.RequestSize = requestSize.Int64
		r.ResponseSize = responseSize.Int64
		r.InputTokens = int(inputTokens.Int64)
		r.OutputTokens = int(outputTokens.Int64)
		r.CacheReadTokens = int(cacheReadTokens.Int64)
		r.CacheWriteTokens = int(cacheWriteTokens.Int64)
		r.TotalTokens = int(totalTokens.Int64)
		r.RetryCount = int(retryCount.Int64)

		records = append(records, r)
	}

	return records, total
}

func GetRecord(id string) *RequestRecord {
	var r RequestRecord
	var timestamp int64
	var reqHeaders, respHeaders sql.NullString
	var method, path, clientIP, keyID, provider, model, userModel sql.NullString
	var statusCode, durationMs, inputTokens, outputTokens sql.NullInt64
	var cacheReadTokens, cacheWriteTokens, totalTokens sql.NullInt64
	var requestBody, responseBody sql.NullString
	var requestSize, responseSize sql.NullInt64

	err := db.QueryRow(`
		SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
			status_code, duration_ms, request_body, response_body, request_headers, response_headers,
			request_size, response_size, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, retry_count
		FROM history WHERE id = ?`, id).Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
		&statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
		&requestSize, &responseSize, &inputTokens, &outputTokens,
		&cacheReadTokens, &cacheWriteTokens, &totalTokens, &r.RetryCount)
	if err != nil {
		return nil
	}

	r.Timestamp = time.Unix(0, timestamp)
	r.Method = method.String
	r.Path = path.String
	r.ClientIP = clientIP.String
	r.KeyID = keyID.String
	r.Provider = provider.String
	r.Model = model.String
	r.UserModel = userModel.String
	r.StatusCode = int(statusCode.Int64)
	r.Duration = durationMs.Int64
	r.RequestBody = requestBody.String
	r.ResponseBody = responseBody.String
	r.RequestSize = requestSize.Int64
	r.ResponseSize = responseSize.Int64
	r.InputTokens = int(inputTokens.Int64)
	r.OutputTokens = int(outputTokens.Int64)
	r.CacheReadTokens = int(cacheReadTokens.Int64)
	r.CacheWriteTokens = int(cacheWriteTokens.Int64)
	r.TotalTokens = int(totalTokens.Int64)

	if reqHeaders.Valid {
		json.Unmarshal([]byte(reqHeaders.String), &r.RequestHeaders)
	}
	if respHeaders.Valid {
		json.Unmarshal([]byte(respHeaders.String), &r.ResponseHeaders)
	}

	return &r
}
