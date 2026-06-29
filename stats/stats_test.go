package stats

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

// setupTestDB 创建一个临时 stats 数据库并替换包级 db 变量。
func setupTestDB(t *testing.T) func() {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "stats.db")

	testDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := testDB.Exec(schemaSQL); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	prev := db
	db = testDB

	return func() {
		testDB.Close()
		db = prev
	}
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS usage_records (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider_id TEXT NOT NULL,
	provider_name TEXT,
	model TEXT,
	input_tokens INTEGER,
	output_tokens INTEGER,
	total_tokens INTEGER,
	cost REAL,
	duration_ms INTEGER,
	time_to_first_ms INTEGER,
	timestamp INTEGER NOT NULL,
	group_name TEXT,
	type_name TEXT,
	key_id TEXT,
	client_ip TEXT,
	user_model TEXT DEFAULT ''
);

CREATE TABLE IF NOT EXISTS provider_stats (
	provider_id TEXT PRIMARY KEY,
	provider_name TEXT,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	request_count INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS key_stats (
	key_id TEXT PRIMARY KEY,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	ip_addresses TEXT DEFAULT '[]',
	request_count INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS key_daily_stats (
	key_id TEXT NOT NULL,
	date TEXT NOT NULL,
	request_count INTEGER DEFAULT 0,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	PRIMARY KEY (key_id, date)
);
`

// trimUsageRecordsToLimit 模拟 RecordUsage 中维持 1000 行上限的 DELETE，
// 让测试场景与生产代码一致。
func trimUsageRecordsToLimit(t *testing.T, limit int) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM usage_records WHERE id NOT IN (SELECT id FROM usage_records ORDER BY timestamp DESC LIMIT ?)`, limit); err != nil {
		t.Fatalf("trim usage_records: %v", err)
	}
}

// TestGetSummary_TotalRequestCountAbove1000 回归测试：总数超过 1000 时，GetSummary
// 不应卡在 usage_records 的 1000 行截断上。
func TestGetSummary_TotalRequestCountAbove1000(t *testing.T) {
	defer setupTestDB(t)()

	const total = 1500
	now := time.Now().UnixNano()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < total; i++ {
		if _, err := tx.Exec(`
			INSERT INTO usage_records (provider_id, provider_name, model, input_tokens,
				output_tokens, total_tokens, cost, duration_ms, time_to_first_ms,
				timestamp, group_name, type_name, key_id, client_ip)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"prov-A", "ProviderA", "model-x", 10, 5, 15, 0.001,
			100, 50, now, "g", "t", "key-A", "127.0.0.1"); err != nil {
			t.Fatalf("insert usage_records[%d]: %v", i, err)
		}
		if _, err := tx.Exec(`
			INSERT INTO provider_stats (provider_id, provider_name, input_tokens,
				output_tokens, total_tokens, total_cost, request_count)
			VALUES (?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(provider_id) DO UPDATE SET
				input_tokens = input_tokens + excluded.input_tokens,
				output_tokens = output_tokens + excluded.output_tokens,
				total_tokens = total_tokens + excluded.total_tokens,
				total_cost = total_cost + excluded.total_cost,
				request_count = request_count + 1`,
			"prov-A", "ProviderA", 10, 5, 15, 0.001); err != nil {
			t.Fatalf("upsert provider_stats[%d]: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	trimUsageRecordsToLimit(t, 1000)

	summary := GetStats().GetSummary()

	if got := summary["total_request_count"]; got != total {
		t.Fatalf("total_request_count = %v, want %d (regression: COUNT(*) on usage_records caps at 1000)", got, total)
	}
	if got := summary["total_input_tokens"]; got != total*10 {
		t.Fatalf("total_input_tokens = %v, want %d", got, total*10)
	}
}

// TestGetTodaySummary_RequestCountAbove1000 回归测试：今日请求数同样不能被 1000 行截断限制。
func TestGetTodaySummary_RequestCountAbove1000(t *testing.T) {
	defer setupTestDB(t)()

	const todayTotal = 1200
	const yesterdayTotal = 200
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixNano()
	yesterdayTs := todayStart - int64(12*time.Hour)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < todayTotal; i++ {
		if _, err := tx.Exec(`
			INSERT INTO usage_records (provider_id, provider_name, model, input_tokens,
				output_tokens, total_tokens, cost, duration_ms, time_to_first_ms,
				timestamp, group_name, type_name, key_id, client_ip)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"prov-B", "ProviderB", "model-x", 1, 1, 2, 0.001,
			100, 50, now.UnixNano(), "g", "t", "key-B", "127.0.0.1"); err != nil {
			t.Fatalf("insert usage_records[%d]: %v", i, err)
		}
	}
	for i := 0; i < yesterdayTotal; i++ {
		if _, err := tx.Exec(`
			INSERT INTO usage_records (provider_id, provider_name, model, input_tokens,
				output_tokens, total_tokens, cost, duration_ms, time_to_first_ms,
				timestamp, group_name, type_name, key_id, client_ip)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"prov-B", "ProviderB", "model-x", 1, 1, 2, 0.001,
			100, 50, yesterdayTs, "g", "t", "key-B", "127.0.0.1"); err != nil {
			t.Fatalf("insert usage_records[y%d]: %v", i, err)
		}
	}
	todayDate := now.Format("2006-01-02")
	if _, err := tx.Exec(`
		INSERT INTO key_daily_stats (key_id, date, request_count, input_tokens, output_tokens, total_cost)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id, date) DO UPDATE SET
			request_count = excluded.request_count,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			total_cost = excluded.total_cost`,
		"key-B", todayDate, todayTotal, todayTotal, todayTotal, float64(todayTotal)*0.001); err != nil {
		t.Fatalf("seed key_daily_stats: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	trimUsageRecordsToLimit(t, 1000)

	today := GetStats().GetTodaySummary()

	if got := today["total_request_count"]; got != todayTotal {
		t.Fatalf("today total_request_count = %v, want %d", got, todayTotal)
	}
	if got := today["total_input_tokens"]; got != todayTotal {
		t.Fatalf("today total_input_tokens = %v, want %d", got, todayTotal)
	}
}

// TestGetDailyHistory_OneDayAbove1000 回归测试：单日历史不应被 1000 行截断。
func TestGetDailyHistory_OneDayAbove1000(t *testing.T) {
	defer setupTestDB(t)()

	const dayTotal = 1300
	now := time.Now()
	todayDate := now.Format("2006-01-02")
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixNano()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < dayTotal; i++ {
		if _, err := tx.Exec(`
			INSERT INTO usage_records (provider_id, provider_name, model, input_tokens,
				output_tokens, total_tokens, cost, duration_ms, time_to_first_ms,
				timestamp, group_name, type_name, key_id, client_ip)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"prov-C", "ProviderC", "model-x", 2, 1, 3, 0.001,
			100, 50, dayStart+int64(i), "g", "t", "key-C", "127.0.0.1"); err != nil {
			t.Fatalf("insert usage_records[%d]: %v", i, err)
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO key_daily_stats (key_id, date, request_count, input_tokens, output_tokens, total_cost)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id, date) DO UPDATE SET
			request_count = excluded.request_count,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			total_cost = excluded.total_cost`,
		"key-C", todayDate, dayTotal, dayTotal*2, dayTotal, float64(dayTotal)*0.001); err != nil {
		t.Fatalf("seed key_daily_stats: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	trimUsageRecordsToLimit(t, 1000)

	hist := GetStats().GetDailyHistory()
	if len(hist) == 0 {
		t.Fatalf("GetDailyHistory returned empty")
	}

	var todayEntry *DailyStats
	for i := range hist {
		if hist[i].Date == todayDate {
			todayEntry = &hist[i]
			break
		}
	}
	if todayEntry == nil {
		t.Fatalf("no entry for today (%s) in history", todayDate)
	}
	if todayEntry.RequestCount != dayTotal {
		t.Fatalf("today RequestCount = %d, want %d", todayEntry.RequestCount, dayTotal)
	}
	if todayEntry.InputTokens != dayTotal*2 {
		t.Fatalf("today InputTokens = %d, want %d", todayEntry.InputTokens, dayTotal*2)
	}
}

// TestRecordUsage_PersistsUserModel 守护：RecordUsage 必须把用户原始模型名（userModel）落库，
// stats→history 的二级广播链路会从这里取 user_model 字段推到 WebSocket 客户端。
func TestRecordUsage_PersistsUserModel(t *testing.T) {
	defer setupTestDB(t)()

	// 其他测试只读 DB 不调 RecordUsage，包级 stats 变量在 Init() 之外默认是 nil；
	// 这里临时初始化一个，让 RecordUsage 末尾的 stats.broadcast <- record 不 panic。
	prevStats := stats
	stats = &Stats{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan UsageRecord, 100),
	}
	t.Cleanup(func() { stats = prevStats })

	RecordUsage("prov-um", "ProviderUM", "actual-model", "my-alias", "g", "t",
		1, 1, 0.001, 100, 50, "key-um", "127.0.0.1")

	var got string
	err := db.QueryRow(`SELECT user_model FROM usage_records WHERE provider_id = ?`, "prov-um").Scan(&got)
	if err != nil {
		t.Fatalf("query user_model: %v (schema must include user_model column)", err)
	}
	if got != "my-alias" {
		t.Errorf("user_model = %q, want my-alias", got)
	}
}

// withTestStats 临时把包级 stats 替换为带 broadcast channel 的实例，
// 让 RecordUsage 末尾的 stats.broadcast <- record 不 panic；返回清理函数。
// 调用方需先 setupTestDB。
func withTestStats(t *testing.T) func() {
	t.Helper()
	prevStats := stats
	stats = &Stats{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan UsageRecord, 100),
	}
	return func() { stats = prevStats }
}

// TestRecordUsage_PersistsKeyDailyTokens 守护：RecordUsage 必须把 input_tokens /
// output_tokens 也累计到 key_daily_stats。GetTodaySummary 顶层今日 Token 计数依赖
// 该列；不写则今日输入/输出 Token 永远 0。
func TestRecordUsage_PersistsKeyDailyTokens(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	RecordUsage("prov-X", "ProviderX", "m", "alias", "g", "t",
		10, 5, 0.1, 100, 50, "key-X", "127.0.0.1")
	RecordUsage("prov-X", "ProviderX", "m", "alias", "g", "t",
		30, 20, 0.2, 200, 80, "key-Y", "127.0.0.1")

	summary := GetStats().GetTodaySummary()
	if got := summary["total_input_tokens"]; got != 40 {
		t.Errorf("total_input_tokens = %v, want 40", got)
	}
	if got := summary["total_output_tokens"]; got != 25 {
		t.Errorf("total_output_tokens = %v, want 25", got)
	}
}

// TestGetSummary_PerKeyTodayFromRecordUsage 守护：GetSummary 返回的 key_stats
// 应当 LEFT JOIN key_daily_stats 取到 today_req_count / today_cost。
func TestGetSummary_PerKeyTodayFromRecordUsage(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	RecordUsage("prov-A", "ProviderA", "m", "alias", "g", "t",
		10, 5, 0.1234, 100, 50, "key-A", "127.0.0.1")
	RecordUsage("prov-A", "ProviderA", "m", "alias", "g", "t",
		20, 7, 0.05, 200, 80, "key-A", "127.0.0.1")

	ksArr, ok := GetStats().GetSummary()["key_stats"].([]*KeyStats)
	if !ok || len(ksArr) == 0 {
		t.Fatalf("expected key_stats array, got %v", GetStats().GetSummary()["key_stats"])
	}
	var got *KeyStats
	for _, k := range ksArr {
		if k.KeyID == "key-A" {
			got = k
			break
		}
	}
	if got == nil {
		t.Fatalf("no entry for key-A in key_stats: %+v", ksArr)
	}
	if got.TodayReqCount != 2 {
		t.Errorf("TodayReqCount = %d, want 2", got.TodayReqCount)
	}
	if got.TodayCost < 0.1733 || got.TodayCost > 0.1735 {
		t.Errorf("TodayCost = %f, want ~0.1734", got.TodayCost)
	}
}

// TestGetTodaySummary_PerKeyToday 守护：GetTodaySummary 的 key_stats 也应 LEFT
// JOIN key_daily_stats 取到 today_req_count / today_cost（与 GetSummary 一致）。
func TestGetTodaySummary_PerKeyToday(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	RecordUsage("prov-A", "ProviderA", "m", "alias", "g", "t",
		10, 5, 0.1234, 100, 50, "key-A", "127.0.0.1")
	RecordUsage("prov-A", "ProviderA", "m", "alias", "g", "t",
		20, 7, 0.05, 200, 80, "key-A", "127.0.0.1")

	ksArr, ok := GetStats().GetTodaySummary()["key_stats"].([]*KeyStats)
	if !ok || len(ksArr) == 0 {
		t.Fatalf("expected key_stats array, got %v", GetStats().GetTodaySummary()["key_stats"])
	}
	var got *KeyStats
	for _, k := range ksArr {
		if k.KeyID == "key-A" {
			got = k
			break
		}
	}
	if got == nil {
		t.Fatalf("no entry for key-A in key_stats: %+v", ksArr)
	}
	if got.TodayReqCount != 2 {
		t.Errorf("GetTodaySummary TodayReqCount = %d, want 2", got.TodayReqCount)
	}
	if got.TodayCost < 0.1733 || got.TodayCost > 0.1735 {
		t.Errorf("GetTodaySummary TodayCost = %f, want ~0.1734", got.TodayCost)
	}
}
