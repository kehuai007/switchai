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
	if m.ID == "" {
		t.Errorf("expected mapping ID assigned")
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
	cfg.Load()
	cfg.AddProvider(Provider{ID: "p1", Name: "P1", BaseURL: "x", APIKey: "k", Model: "X", IsActive: true, CreatedAt: "now", Order: 1})
	cfg.AddServerKey(ServerKey{Key: "sk-1", IsEnabled: true, Order: 1})

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