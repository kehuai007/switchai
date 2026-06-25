package config

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestProvider_ChatEndpointURL(t *testing.T) {
	tests := []struct {
		name string
		p    *Provider
		want string
	}{
		{"openai with /v1 suffix", &Provider{BaseURL: "https://api.openai.com/v1", IsOpenAIFormat: true}, "https://api.openai.com/v1/chat/completions"},
		{"openai with /v1 suffix + trailing slash", &Provider{BaseURL: "https://api.openai.com/v1/", IsOpenAIFormat: true}, "https://api.openai.com/v1/chat/completions"},
		{"openai without /v1 suffix", &Provider{BaseURL: "https://api.openai.com", IsOpenAIFormat: true}, "https://api.openai.com/v1/chat/completions"},
		{"anthropic with /v1 suffix", &Provider{BaseURL: "https://api.minimaxi.com/v1", IsOpenAIFormat: false}, "https://api.minimaxi.com/v1/messages"},
		{"anthropic without /v1 suffix", &Provider{BaseURL: "https://api.anthropic.com", IsOpenAIFormat: false}, "https://api.anthropic.com/v1/messages"},
		// 守护 web.testProvider 和 proxy.format-conversion 路径之前 naive 拼接导致的 /v1/v1/messages 重复 bug
		{"anthropic with /v1 suffix (regression)", &Provider{BaseURL: "https://api.minimaxi.com/v1", IsOpenAIFormat: false}, "https://api.minimaxi.com/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.ChatEndpointURL(); got != tt.want {
				t.Errorf("ChatEndpointURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildProviderURL 守护 URL 拼接逻辑：避免出现 /v1/v1 或 // 这类非法 URL。
// 覆盖 trailing slash、连续 trailing slash、缺前导 /、纯 BaseURL 等常见 base_url 形态。
func TestBuildProviderURL(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		// 标准形态
		{"no v1, no trailing slash", "https://api.example.com", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"with v1, no trailing slash", "https://api.example.com/v1", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		// trailing slash 各种形态 — 都不应引入 //
		{"trailing slash, no v1", "https://api.example.com/", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"trailing slash, with v1", "https://api.example.com/v1/", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"double trailing slash, with v1", "https://api.example.com/v1//", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"triple trailing slash, with v1", "https://api.example.com/v1///", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		// endpoint 容忍无前导 /
		{"endpoint without leading slash", "https://api.example.com", "chat/completions", "https://api.example.com/v1/chat/completions"},
		{"endpoint without leading slash, with v1", "https://api.example.com/v1", "chat/completions", "https://api.example.com/v1/chat/completions"},
		// 多种 endpoint
		{"messages endpoint, no v1", "https://api.example.com", "/messages", "https://api.example.com/v1/messages"},
		{"models endpoint, with v1", "https://api.example.com/v1", "/models", "https://api.example.com/v1/models"},
		// 守护具体的 /v1/v1 bug
		{"anthropic-style + /v1 suffix must not duplicate v1", "https://api.minimaxi.com/v1", "/messages", "https://api.minimaxi.com/v1/messages"},
		// 中间路径含 v1 但末尾不是 v1（如 /api/v1）— 这时 API 根在该位置，直接拼接
		{"baseURL path is /api/v1", "https://api.example.com/api/v1", "/messages", "https://api.example.com/api/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildProviderURL(tt.baseURL, tt.endpoint)
			if got != tt.want {
				t.Errorf("BuildProviderURL(%q, %q) = %q, want %q", tt.baseURL, tt.endpoint, got, tt.want)
			}
			// 拼接结果不应包含 // 或 /v1/v1 这类非法片段
			if strings.Contains(got, "/v1/v1") {
				t.Errorf("BuildProviderURL(%q, %q) = %q, contains illegal /v1/v1", tt.baseURL, tt.endpoint, got)
			}
			if strings.Contains(got, "//") {
				// 排除 scheme 自带的 https://
				stripped := strings.TrimPrefix(got, "https://")
				stripped = strings.TrimPrefix(stripped, "http://")
				if strings.Contains(stripped, "//") {
					t.Errorf("BuildProviderURL(%q, %q) = %q, contains illegal //", tt.baseURL, tt.endpoint, got)
				}
			}
		})
	}
}

func TestProvider_GetSupportedModels(t *testing.T) {
	tests := []struct {
		name string
		p    *Provider
		want []string
	}{
		{"empty", &Provider{Model: ""}, nil},
		{"single value (backward compat)", &Provider{Model: "claude-sonnet-4-5"}, []string{"claude-sonnet-4-5"}},
		{"semicolon multi", &Provider{Model: "X;Y;Z"}, []string{"X", "Y", "Z"}},
		{"trim whitespace", &Provider{Model: " X ; Y "}, []string{"X", "Y"}},
		{"filter empty segments", &Provider{Model: "X;;Y"}, []string{"X", "Y"}},
		{"single with spaces", &Provider{Model: "  claude-sonnet-4-5  "}, []string{"claude-sonnet-4-5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.GetSupportedModels(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetSupportedModels() = %v, want %v", got, tt.want)
			}
		})
	}
}

// setupTestDB 创建一个临时 config 数据库并替换包级 db 变量。
func setupTestDB(t *testing.T) func() {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "config.db")

	testDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := testDB.Exec(testSchemaSQL); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	prev := db
	db = testDB

	return func() {
		testDB.Close()
		db = prev
	}
}

const testSchemaSQL = `
CREATE TABLE IF NOT EXISTS config (
	key TEXT PRIMARY KEY,
	value TEXT
);
CREATE TABLE IF NOT EXISTS providers (
	id TEXT PRIMARY KEY,
	name TEXT,
	base_url TEXT,
	api_key TEXT,
	model TEXT,
	is_active INTEGER,
	created_at TEXT,
	order_num INTEGER,
	is_openai_format INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS server_keys (
	id TEXT PRIMARY KEY,
	key TEXT,
	remark TEXT,
	is_enabled INTEGER,
	created_at TEXT,
	order_num INTEGER,
	daily_req_limit INTEGER DEFAULT 0,
	total_req_limit INTEGER DEFAULT 0,
	daily_cost_limit REAL DEFAULT 0,
	total_cost_limit REAL DEFAULT 0
);
-- model_mappings.provider_id 无 FK 约束：provider 删除由 web 层拦截
CREATE TABLE IF NOT EXISTS model_mappings (
	id TEXT PRIMARY KEY,
	server_key_id TEXT NOT NULL,
	user_model TEXT NOT NULL,
	provider_id TEXT NOT NULL,
	provider_model TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE(server_key_id, user_model),
	FOREIGN KEY (server_key_id) REFERENCES server_keys(id) ON DELETE CASCADE
);
`

func TestConfig_AddMapping_GetMappingForRouting(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// seed 一个 provider 和一个 server_key
	p := Provider{
		ID: "p1", Name: "P1", BaseURL: "http://x", APIKey: "k",
		Model: "X;Y;Z", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1,
	}
	if err := cfg.AddProvider(p); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	sk := ServerKey{Key: "sk-test", Remark: "test", IsEnabled: true, Order: 1}
	if err := cfg.AddServerKey(sk); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}

	// 添加映射
	m, err := cfg.AddMapping(sk.ID, ModelMapping{
		UserModel: "claude-sonnet-4-5", ProviderID: p.ID, ProviderModel: "Y",
	})
	if err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if m.ID == "" || m.UserModel != "claude-sonnet-4-5" || m.ServerKeyID != sk.ID || m.ProviderID != p.ID || m.ProviderModel != "Y" || m.CreatedAt == "" {
		t.Errorf("AddMapping returned incomplete mapping: %+v", m)
	}

	// 查路由
	mapping, provider, err := cfg.GetMappingForRouting(sk.ID, "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("GetMappingForRouting: %v", err)
	}
	if mapping.ProviderModel != "Y" {
		t.Errorf("got ProviderModel=%q, want Y", mapping.ProviderModel)
	}
	if provider.ID != p.ID {
		t.Errorf("got provider ID=%q, want %q", provider.ID, p.ID)
	}
}

func TestConfig_AddMapping_DuplicateUserModel(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}

	keyID := lookupKeyIDByKey(cfg, "sk-1")
	if _, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "p1", ProviderModel: "X"}); err != nil {
		t.Fatalf("first AddMapping: %v", err)
	}
	_, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "p1", ProviderModel: "X"})
	if err == nil {
		t.Errorf("expected error on duplicate user_model, got nil")
	}
}

func lookupKeyIDByKey(cfg *Config, key string) string {
	for _, k := range cfg.GetServerKeys() {
		if k.Key == key {
			return k.ID
		}
	}
	return ""
}

func TestConfig_GetMappingForRouting_NotFound(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")

	_, _, err := cfg.GetMappingForRouting(keyID, "no-such-model")
	if err == nil {
		t.Errorf("expected error for missing mapping")
	}
}

func TestConfig_GetMappingForRouting_ProviderInactive(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: false, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")
	if _, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "p1", ProviderModel: "X"}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}

	_, _, err := cfg.GetMappingForRouting(keyID, "A")
	if err == nil {
		t.Errorf("expected error for inactive provider")
	}
}

func TestConfig_GetMappingForRouting_ProviderMissing(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")
	if _, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "ghost-p", ProviderModel: "X"}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}

	_, _, err := cfg.GetMappingForRouting(keyID, "A")
	if err == nil {
		t.Errorf("expected error for missing provider")
	}
}

// TestConfig_Load_DoesNotDeadlock 守护 Load() 不会因为重入 c.mu 死锁。
// 历史：commit e5fdd2c 在 Load() 持写锁时调 LoadMappingsForKey (内部 RLock)，
// sync.RWMutex 不可重入，导致启动永远卡住。
func TestConfig_Load_DoesNotDeadlock(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}

	done := make(chan error, 1)
	go func() {
		done <- cfg.Load()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Load returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Load deadlocked — same goroutine took write lock and tried to RLock via LoadMappingsForKey")
	}
}

// TestConfig_GetActiveMappingsForKey_FiltersInactiveProviders 守护 GetActiveMappingsForKey
// 只返回目标 provider 处于 active 状态的映射 —— 否则禁用 provider 的模型也会列在
// /v1/models 里，但实际请求会 500/报错，前后端不一致。
func TestConfig_GetActiveMappingsForKey_FiltersInactiveProviders(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	pActive := Provider{ID: "p-active", Name: "Active", BaseURL: "x", APIKey: "k",
		Model: "X", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}
	pInactive := Provider{ID: "p-inactive", Name: "Inactive", BaseURL: "x", APIKey: "k",
		Model: "Y", IsActive: false, CreatedAt: time.Now().Format(time.RFC3339), Order: 2}
	if err := cfg.AddProvider(pActive); err != nil {
		t.Fatalf("AddProvider active: %v", err)
	}
	if err := cfg.AddProvider(pInactive); err != nil {
		t.Fatalf("AddProvider inactive: %v", err)
	}

	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")

	now := time.Now().Format(time.RFC3339)
	if _, err := cfg.AddMapping(keyID, ModelMapping{
		UserModel: "u-active", ProviderID: "p-active", ProviderModel: "target-X",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("AddMapping active: %v", err)
	}
	if _, err := cfg.AddMapping(keyID, ModelMapping{
		UserModel: "u-inactive", ProviderID: "p-inactive", ProviderModel: "target-Y",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("AddMapping inactive: %v", err)
	}

	got := cfg.GetActiveMappingsForKey(keyID)
	if len(got) != 1 {
		t.Fatalf("got %d mappings, want 1 (inactive provider's mapping must be filtered out): %+v", len(got), got)
	}
	if got[0].UserModel != "u-active" || got[0].ProviderModel != "target-X" {
		t.Errorf("got %+v, want user_model=u-active provider_model=target-X", got[0])
	}
}

// TestConfig_GetActiveMappingsForKey_UnknownKey / 空映射键 返回长度 0（nil 或空 slice 都接受）。
func TestConfig_GetActiveMappingsForKey_UnknownKey(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.GetActiveMappingsForKey("nonexistent-key-id")
	if len(got) != 0 {
		t.Errorf("got %d mappings, want 0", len(got))
	}
}

// TestConfig_UpdateServerKey_PreservesMappings 验证 UpdateServerKey 不会清空内存里
// 已加载的 Mappings — 前端编辑弹窗保存时只发 remark/is_enabled/限额等字段，不发 mappings，
// 若后端整体替换 ServerKey，会把内存里的 mappings 清掉，导致 GET /api/server-keys
// 返回 mappings:null（虽然 DB 里仍然有映射，/v1/models 也能正常返回）。
func TestConfig_UpdateServerKey_PreservesMappings(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Remark: "old", Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")

	if _, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "p1", ProviderModel: "X"}); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if got := cfg.GetServerKeys()[0].Mappings; len(got) != 1 {
		t.Fatalf("precondition: expected 1 mapping in memory, got %d", len(got))
	}

	// 前端保存编辑表单时只发这些字段，没有 mappings
	update := ServerKey{IsEnabled: false, Remark: "new"}
	if err := cfg.UpdateServerKey(keyID, update); err != nil {
		t.Fatalf("UpdateServerKey: %v", err)
	}

	got := cfg.GetServerKeys()[0]
	if len(got.Mappings) != 1 || got.Mappings[0].UserModel != "A" {
		t.Fatalf("after UpdateServerKey, Mappings wiped: got %+v, want the original mapping preserved", got.Mappings)
	}
	if got.Remark != "new" || got.IsEnabled != false {
		t.Fatalf("UpdateServerKey did not apply new fields: remark=%q enabled=%v", got.Remark, got.IsEnabled)
	}
}

// TestConfig_MappingCRUD_KeepsInMemoryMappingsInSync 验证 AddMapping/UpdateMapping/DeleteMapping
// 会同步更新内存中 ServerKey.Mappings，否则 GetServerKeys() 会返回过期数据（前端刷不出来）。
func TestConfig_MappingCRUD_KeepsInMemoryMappingsInSync(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	cfg := &Config{}
	if err := cfg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: true, CreatedAt: time.Now().Format(time.RFC3339), Order: 1}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1}); err != nil {
		t.Fatalf("AddServerKey: %v", err)
	}
	keyID := lookupKeyIDByKey(cfg, "sk-1")

	// 初始状态：空 mappings
	if got := cfg.GetServerKeys()[0].Mappings; len(got) != 0 {
		t.Fatalf("initial mappings should be empty, got %d", len(got))
	}

	// AddMapping 应同步到内存
	created, err := cfg.AddMapping(keyID, ModelMapping{UserModel: "A", ProviderID: "p1", ProviderModel: "X"})
	if err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if got := cfg.GetServerKeys()[0].Mappings; len(got) != 1 || got[0].ID != created.ID {
		t.Fatalf("after AddMapping, GetServerKeys().Mappings = %+v, want 1 entry with ID %q", got, created.ID)
	}

	// UpdateMapping 应同步到内存
	if err := cfg.UpdateMapping(keyID, created.ID, ModelMapping{UserModel: "B", ProviderID: "p1", ProviderModel: "X"}); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	if got := cfg.GetServerKeys()[0].Mappings; len(got) != 1 || got[0].UserModel != "B" {
		t.Fatalf("after UpdateMapping, Mappings[0].UserModel = %q, want B", got[0].UserModel)
	}

	// DeleteMapping 应同步到内存
	if err := cfg.DeleteMapping(keyID, created.ID); err != nil {
		t.Fatalf("DeleteMapping: %v", err)
	}
	if got := cfg.GetServerKeys()[0].Mappings; len(got) != 0 {
		t.Fatalf("after DeleteMapping, Mappings should be empty, got %d", len(got))
	}
}
