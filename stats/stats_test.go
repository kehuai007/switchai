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

CREATE TABLE IF NOT EXISTS provider_token_history (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	provider_id   TEXT    NOT NULL,
	t_bucket      INTEGER NOT NULL,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens  INTEGER NOT NULL DEFAULT 0,
	request_count INTEGER NOT NULL DEFAULT 0,
	UNIQUE(provider_id, t_bucket)
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

// TestGetKeyTodayBuckets_5h 守护：bucket=5h 应把今日 00:00-04:59 归为桶 1，
// 05:00-09:59 归为桶 2，依此类推。末段 20:00-23:59 是 4h。
func TestGetKeyTodayBuckets_5h(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// 桶 1：01:00 → input=10, output=5, cost=0.10
	// 桶 2：06:00 → input=20, output=8, cost=0.20
	// 桶 3：12:00 → input=30, output=12, cost=0.30
	// 桶 5：21:00 → input=40, output=15, cost=0.40（4h 段）
	stamp := func(h, m int) int64 {
		return todayStart.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).UnixNano()
	}
	insertUsage := func(key string, ts int64, inTok, outTok int, cost float64) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, outTok, inTok+outTok, cost, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(1, 0), 10, 5, 0.10)
	insertUsage("k1", stamp(6, 0), 20, 8, 0.20)
	insertUsage("k1", stamp(12, 0), 30, 12, 0.30)
	insertUsage("k1", stamp(21, 0), 40, 15, 0.40)

	got, err := GetKeyTodayBuckets("k1", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "5h" {
		t.Errorf("Bucket = %q, want 5h", got.Bucket)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("Buckets length = %d, want 5", len(got.Buckets))
	}
	// 桶 1：01:00 落入 00:00 桶
	if got.Buckets[0].InputTokens != 10 || got.Buckets[0].RequestCount != 1 {
		t.Errorf("桶1 input/count = %d/%d, want 10/1", got.Buckets[0].InputTokens, got.Buckets[0].RequestCount)
	}
	// 桶 2：06:00 落入 05:00 桶
	if got.Buckets[1].InputTokens != 20 {
		t.Errorf("桶2 input = %d, want 20", got.Buckets[1].InputTokens)
	}
	// 桶 3：12:00 落入 10:00 桶
	if got.Buckets[2].InputTokens != 30 {
		t.Errorf("桶3 input = %d, want 30", got.Buckets[2].InputTokens)
	}
	// 桶 4：15:00-19:59 → 应为空
	if got.Buckets[3].RequestCount != 0 {
		t.Errorf("桶4 request_count = %d, want 0", got.Buckets[3].RequestCount)
	}
	// 桶 5：21:00 落入 20:00 桶
	if got.Buckets[4].InputTokens != 40 {
		t.Errorf("桶5 input = %d, want 40", got.Buckets[4].InputTokens)
	}
}

// TestGetKeyTodayBuckets_Hour 守护：bucket=hour 应返回 24 个整点桶。
func TestGetKeyTodayBuckets_Hour(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h int) int64 {
		return todayStart.Add(time.Duration(h) * time.Hour).Add(5 * time.Minute).UnixNano() // hh:05 落入 hh 桶
	}
	insertUsage := func(key string, ts int64, inTok int) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, 0, inTok, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(3), 100)
	insertUsage("k1", stamp(15), 200)
	insertUsage("k1", stamp(23), 300)

	got, err := GetKeyTodayBuckets("k1", "hour")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "hour" {
		t.Errorf("Bucket = %q, want hour", got.Bucket)
	}
	if len(got.Buckets) != 24 {
		t.Fatalf("Buckets length = %d, want 24", len(got.Buckets))
	}
	if got.Buckets[3].InputTokens != 100 {
		t.Errorf("桶 03:00 input = %d, want 100", got.Buckets[3].InputTokens)
	}
	if got.Buckets[15].InputTokens != 200 {
		t.Errorf("桶 15:00 input = %d, want 200", got.Buckets[15].InputTokens)
	}
	if got.Buckets[23].InputTokens != 300 {
		t.Errorf("桶 23:00 input = %d, want 300", got.Buckets[23].InputTokens)
	}
	// 其他桶为空
	if got.Buckets[0].RequestCount != 0 || got.Buckets[10].RequestCount != 0 {
		t.Errorf("空桶不为 0: 桶0=%d, 桶10=%d", got.Buckets[0].RequestCount, got.Buckets[10].RequestCount)
	}
}

// TestGetKeyTodayBuckets_Empty 守护：key 今日无请求时，buckets 数组仍按完整长度返回（全 0）。
func TestGetKeyTodayBuckets_Empty(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	got, err := GetKeyTodayBuckets("nonexistent", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("5h buckets length = %d, want 5", len(got.Buckets))
	}
	for i, b := range got.Buckets {
		if b.RequestCount != 0 || b.InputTokens != 0 || b.OutputTokens != 0 || b.Cost != 0 {
			t.Errorf("桶 %d 不为空: %+v", i, b)
		}
	}

	gotHour, err := GetKeyTodayBuckets("nonexistent", "hour")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets hour: %v", err)
	}
	if len(gotHour.Buckets) != 24 {
		t.Errorf("hour buckets length = %d, want 24", len(gotHour.Buckets))
	}
}

// TestGetKeyTodayBuckets_InvalidBucket 守护：非法 bucket 值应 fallback 到 5h。
func TestGetKeyTodayBuckets_InvalidBucket(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	got, err := GetKeyTodayBuckets("k1", "day")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "5h" {
		t.Errorf("Bucket = %q, want 5h (fallback)", got.Bucket)
	}
	if len(got.Buckets) != 5 {
		t.Errorf("Buckets length = %d, want 5", len(got.Buckets))
	}
}

// TestGetKeyTodayBuckets_Boundary 守护：跨 5h/小时边界的请求必须落到正确桶。
// 5h 边界：04:59:59 落入桶 1，05:00:00 落入桶 2。
// 小时边界：00:59:59 落入桶 0，01:00:00 落入桶 1。
func TestGetKeyTodayBuckets_Boundary(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h, m, s int) int64 {
		return todayStart.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second).UnixNano()
	}
	insertUsage := func(key string, ts int64, tag string) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", 1, 0, 1, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert %s: %v", tag, err)
		}
	}
	// 5h 边界用独立 key k5h，避免和 hour 边界记录混到同一 5h 桶
	insertUsage("k5h", stamp(4, 59, 59), "4:59:59")  // 5h 桶 1 (00:00-04:59)
	insertUsage("k5h", stamp(5, 0, 0), "5:00:00")    // 5h 桶 2 (05:00-09:59)
	// hour 边界用独立 key kh
	insertUsage("kh", stamp(0, 59, 59), "0:59:59")   // hour 桶 0 (00:00-00:59)
	insertUsage("kh", stamp(1, 0, 0), "1:00:00")     // hour 桶 1 (01:00-01:59)

	// 5h
	got5h, _ := GetKeyTodayBuckets("k5h", "5h")
	if got5h.Buckets[0].RequestCount != 1 {
		t.Errorf("5h 桶 1 (04:59:59) request_count = %d, want 1", got5h.Buckets[0].RequestCount)
	}
	if got5h.Buckets[1].RequestCount != 1 {
		t.Errorf("5h 桶 2 (05:00:00) request_count = %d, want 1", got5h.Buckets[1].RequestCount)
	}

	// hour
	gotHour, _ := GetKeyTodayBuckets("kh", "hour")
	if gotHour.Buckets[0].RequestCount != 1 {
		t.Errorf("hour 桶 0 (00:59:59) request_count = %d, want 1", gotHour.Buckets[0].RequestCount)
	}
	if gotHour.Buckets[1].RequestCount != 1 {
		t.Errorf("hour 桶 1 (01:00:00) request_count = %d, want 1", gotHour.Buckets[1].RequestCount)
	}
}

// TestGetKeyTodayBuckets_LastBucket 守护：末段 20:00-23:59 是 4h（不补齐为 5h）。
// t 字段是 20:00:00，RequestCount 等数据按实际落入。
func TestGetKeyTodayBuckets_LastBucket(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h int) int64 {
		return todayStart.Add(time.Duration(h) * time.Hour).UnixNano()
	}
	insertUsage := func(key string, ts int64, inTok int) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, 0, inTok, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(22), 50) // 末段

	got, err := GetKeyTodayBuckets("k1", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("Buckets length = %d, want 5 (不补齐末段)", len(got.Buckets))
	}
	if got.Buckets[4].InputTokens != 50 {
		t.Errorf("末段 input = %d, want 50", got.Buckets[4].InputTokens)
	}
	// 末段 t 字段应为 20:00:00
	wantT := todayStart.Add(20 * time.Hour)
	if !got.Buckets[4].T.Equal(wantT) {
		t.Errorf("末段 t = %v, want %v", got.Buckets[4].T, wantT)
	}
}

// TestGetKeyTodayBuckets_7d 验证 7d 桶：返回 7 桶、含今天、按日期升序、缺失日期补 0。
func TestGetKeyTodayBuckets_7d(t *testing.T) {
	defer setupTestDB(t)()

	keyID := "test-key-7d"

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayDate := todayStart.Format("2006-01-02")

	// 写入 7 天数据，但缺失 today-3（验证缺失桶补 0）
	rows := []struct {
		date     string
		input    int
		output   int
		requests int
		cost     float64
	}{
		{todayStart.AddDate(0, 0, -6).Format("2006-01-02"), 100, 50, 1, 0.10},
		{todayStart.AddDate(0, 0, -5).Format("2006-01-02"), 200, 80, 2, 0.20},
		{todayStart.AddDate(0, 0, -4).Format("2006-01-02"), 300, 90, 3, 0.30},
		// today-3 缺失
		{todayStart.AddDate(0, 0, -2).Format("2006-01-02"), 400, 100, 4, 0.40},
		{todayStart.AddDate(0, 0, -1).Format("2006-01-02"), 500, 110, 5, 0.50},
		{todayDate, 600, 120, 6, 0.60},
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO key_daily_stats
			(key_id, date, input_tokens, output_tokens, request_count, total_cost)
			VALUES (?, ?, ?, ?, ?, ?)`,
			keyID, r.date, r.input, r.output, r.requests, r.cost)
		if err != nil {
			t.Fatalf("insert failed for %s: %v", r.date, err)
		}
	}

	stats, err := GetKeyTodayBuckets(keyID, "7d")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets(7d) returned error: %v", err)
	}

	if stats.Bucket != "7d" {
		t.Fatalf("expected bucket=7d, got %q", stats.Bucket)
	}
	if len(stats.Buckets) != 7 {
		t.Fatalf("expected 7 buckets, got %d", len(stats.Buckets))
	}

	// 验证升序：i=0 → today-6, i=6 → today
	for i, b := range stats.Buckets {
		expectedDate := todayStart.AddDate(0, 0, -(6 - i)).Format("2006-01-02")
		if got := b.T.Format("2006-01-02"); got != expectedDate {
			t.Errorf("bucket[%d].T = %s, want %s", i, got, expectedDate)
		}
	}

	// today-3 缺失桶应为 0（i=3 → today-3）
	missing := stats.Buckets[3]
	if missing.InputTokens != 0 || missing.OutputTokens != 0 ||
		missing.RequestCount != 0 || missing.Cost != 0 {
		t.Errorf("expected zero bucket at i=3, got %+v", missing)
	}

	// 今天桶（i=6）应能正确读出累计数据
	today := stats.Buckets[6]
	if today.InputTokens != 600 || today.OutputTokens != 120 ||
		today.RequestCount != 6 || today.Cost != 0.60 {
		t.Errorf("today bucket mismatch: %+v", today)
	}

	// Date 字段应为今天
	if stats.Date != todayDate {
		t.Errorf("stats.Date = %s, want %s", stats.Date, todayDate)
	}
}

// TestRecordUsage_AccumulatesProviderTokenHistory 守护：RecordUsage 必须在 provider_token_history
// 表里按 10s 桶累计 input/output/total tokens 和 request_count，ON CONFLICT 加和而非覆盖。
// 否则配额历史弹窗拿不到趋势数据。
func TestRecordUsage_AccumulatesProviderTokenHistory(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	pid := "test-provider-token"
	RecordUsage(pid, "TestProv", "m1", "um1", "g", "chat",
		100, 50, 0.01, 200, 100, "k1", "1.2.3.4")
	RecordUsage(pid, "TestProv", "m1", "um1", "g", "chat",
		200, 80, 0.02, 200, 100, "k1", "1.2.3.4")

	tb := (time.Now().UnixNano() / 1e10) * 10

	var in, out, tot, cnt int
	row := db.QueryRow(`SELECT input_tokens, output_tokens, total_tokens, request_count
		FROM provider_token_history WHERE provider_id=? AND t_bucket=?`, pid, tb)
	if err := row.Scan(&in, &out, &tot, &cnt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if in != 300 || out != 130 || tot != 430 || cnt != 2 {
		t.Errorf("want 300/130/430/2 got %d/%d/%d/%d", in, out, tot, cnt)
	}
}
