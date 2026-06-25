package config

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

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
