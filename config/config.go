package config

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"switchai/appdata"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// SessionTokenEntry represents a single session token entry
type SessionTokenEntry struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

type Provider struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	Model          string `json:"model"`
	IsActive       bool   `json:"is_active"`
	CreatedAt      string `json:"created_at"`
	Order          int    `json:"order"`
	IsOpenAIFormat bool   `json:"is_openai_format"` // 标识是否为 OpenAI 格式的 API

	// Quota — 由 quota 包在请求时填充，不持久化（QuotaBlockEnabled 除外，由 Config.QuotaBlockEnabled 单独持久化）。
	QuotaEnabled        bool            `json:"quota_enabled,omitempty"`
	QuotaError          string          `json:"quota_error,omitempty"`
	QuotaInterval       QuotaWindowJSON `json:"quota_interval,omitempty"`
	QuotaWeekly         QuotaWindowJSON `json:"quota_weekly,omitempty"`
	QuotaBlockEnabled   bool            `json:"quota_block_enabled"`
	QuotaBlockThreshold int             `json:"quota_block_threshold"` // 1..100，默认 99
}

// QuotaWindowJSON 描述某一条额度窗口（5h 区间或 7d 本周）的展示字段。
// 由 web 层从 quota.Snapshot 转 json 后挂在 Provider 上返回给前端。
type QuotaWindowJSON struct {
	Enabled          bool    `json:"enabled"`
	RemainingPercent float64 `json:"remaining_percent,omitempty"`
	UsedPercent      float64 `json:"used_percent"`
	StartTime        int64   `json:"start_time,omitempty"` // unix ms
	EndTime          int64   `json:"end_time,omitempty"`   // unix ms
	ResetInSec       int     `json:"reset_in_sec,omitempty"`
	ResetInHuman     string  `json:"reset_in_human,omitempty"`
	TotalCount       int64   `json:"total_count,omitempty"`
	UsageCount       int64   `json:"usage_count,omitempty"`
	Status           int     `json:"status,omitempty"`
}

// BuildProviderURL 拼接 BaseURL 和 endpoint，避免出现 /v1/v1 或 // 这类非法 URL。
//   - BaseURL 末尾的 / 全部去掉（TrimRight），包括连续多个；
//   - endpoint 应以 / 开头（无 / 会自动补），且**相对于 API v1 根**（如 /chat/completions、/messages、/models），
//     不要带 /v1 前缀 —— 这里会根据 BaseURL 是否已含 /v1 决定是否补；
//   - 如果 BaseURL 已经以 /v1 结尾（指向 API v1 根），endpoint 直接拼接；
//   - 否则在 BaseURL 和 endpoint 之间插入 /v1；
//
// 调用方不需要关心上游是否真的暴露 /v1 —— 这里只负责 URL 字符串拼接，协议层语义由 IsOpenAIFormat
// 等配置字段决定（参见 Provider.ChatEndpointURL）。
func BuildProviderURL(baseURL, endpoint string) string {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	if strings.HasSuffix(base, "/v1") {
		return base + endpoint
	}
	return base + "/v1" + endpoint
}

// ChatEndpointURL 返回 provider 主端点（chat completions 或 messages）的完整 URL。
// 由 IsOpenAIFormat 决定选哪条路径（OpenAI 用 /chat/completions，Anthropic 用 /messages），
// URL 拼接复用 BuildProviderURL 以避免 /v1/v1 或 // 这类非法 URL。
func (p *Provider) ChatEndpointURL() string {
	if p.IsOpenAIFormat {
		return BuildProviderURL(p.BaseURL, "/chat/completions")
	}
	return BuildProviderURL(p.BaseURL, "/messages")
}

// GetSupportedModels 解析 Model 字段（"X;Y;Z" 或单值），返回 trim 并过滤空段后的模型名列表。
// 空字符串返回 nil（provider 未声明任何模型）。不去重 — 重复声明视为调用方责任。
func (p *Provider) GetSupportedModels() []string {
	if p.Model == "" {
		return nil
	}
	parts := strings.Split(p.Model, ";")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

type ModelMapping struct {
	ID            string `json:"id"`
	ServerKeyID   string `json:"server_key_id"`
	UserModel     string `json:"user_model"`
	ProviderID    string `json:"provider_id"`
	ProviderModel string `json:"provider_model"`
	CreatedAt     string `json:"created_at"`
}

type ServerKey struct {
	ID               string         `json:"id"`               // 密钥ID
	Key              string         `json:"key"`              // 密钥值 sk-xxxx
	Remark           string         `json:"remark"`           // 备注
	IsEnabled        bool           `json:"is_enabled"`       // 是否启用
	CreatedAt        string         `json:"created_at"`       // 创建时间
	Order            int            `json:"order"`            // 排序序号
	DailyReqLimit    int            `json:"daily_req_limit"`  // 每日请求次数限额 (0=不限)
	TotalReqLimit    int            `json:"total_req_limit"`  // 总请求次数限额 (0=不限)
	DailyCostLimit   float64        `json:"daily_cost_limit"` // 每日花费限额 (0=不限)
	TotalCostLimit   float64        `json:"total_cost_limit"` // 总花费限额 (0=不限)
	Mappings         []ModelMapping `json:"mappings"`
}

type Config struct {
	Providers         []Provider          `json:"providers"`
	ServerKeys        []ServerKey         `json:"server_keys"` // 服务器密钥列表
	TOTPSecret        string              `json:"totp_secret"`  // TOTP 2FA 密钥
	TOTPEnabled       bool                `json:"totp_enabled"` // 是否已启用 2FA
	SessionTokens     []SessionTokenEntry `json:"session_tokens"` // 多端登录的会话 token 列表
	SkipAuth          bool                `json:"skip_auth"`      // 跳过认证（内网部署）
	QuotaBlockEnabled map[string]bool     `json:"-"`              // provider_id -> 是否启用额度拦截；启动时从 DB 重建，O(1) 查询
	mu                sync.RWMutex
}

var skipAuthMode bool

// SetSkipAuth 设置跳过认证模式
func SetSkipAuth(skip bool) {
	skipAuthMode = skip
}

// IsSkipAuth 返回是否跳过认证模式
func IsSkipAuth() bool {
	return skipAuthMode
}

var cfg *Config
var db *sql.DB

// ErrConfiguredProviderMissing is returned when a mapping references a provider_id
// that doesn't exist in the providers list. Use errors.Is to detect.
var ErrConfiguredProviderMissing = errors.New("configured provider missing")

// ErrServerKeyDuplicate is returned when an edited ServerKey.Key collides with
// another existing ServerKey. Use errors.Is to detect.
var ErrServerKeyDuplicate = errors.New("密钥已被其他密钥使用")

// validateServerKeyFormat enforces sk- prefix + 16 chars in [a-zA-Z0-9].
// Used by edit path to keep auto-generated format invariant intact.
func validateServerKeyFormat(key string) error {
	const prefix = "sk-"
	const bodyLen = 16
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("密钥必须以 %q 开头", prefix)
	}
	body := key[len(prefix):]
	if len(body) != bodyLen {
		return fmt.Errorf("密钥主体长度必须为 %d", bodyLen)
	}
	for _, r := range body {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("密钥主体只能包含字母和数字")
		}
	}
	return nil
}

// getDBPath 返回数据库文件路径
func getDBPath() string {
	return appdata.GetConfigPath("config.db")
}

func Init() error {
	// 初始化数据库
	var err error
	dbPath := getDBPath()
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// 创建表
	if err := initDB(); err != nil {
		db.Close()
		return err
	}

	cfg = &Config{}

	// 加载配置
	if err := cfg.Load(); err != nil {
		return err
	}

	return nil
}

func initDB() error {
	schema := `
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
		is_openai_format INTEGER DEFAULT 0,
		quota_block_enabled INTEGER DEFAULT 0,
		quota_block_threshold INTEGER DEFAULT 99
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
	-- model_mappings.provider_id 无 FK 约束：provider 删除由 web 层拦截（见 web.deleteProvider）
	-- 避免级联删除 mappings，让用户能感知到冲突
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
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// 迁移：添加 is_openai_format 列（如果不存在）
	db.Exec("ALTER TABLE providers ADD COLUMN is_openai_format INTEGER DEFAULT 0")
	// 迁移：添加 quota_block_enabled 列（如果不存在）
	db.Exec("ALTER TABLE providers ADD COLUMN quota_block_enabled INTEGER DEFAULT 0")
	// 迁移：添加 quota_block_threshold 列（如果不存在）；默认 99 与现有硬编码行为一致
	db.Exec("ALTER TABLE providers ADD COLUMN quota_block_threshold INTEGER DEFAULT 99")

	return nil
}

func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 加载 totp_secret
	var totpSecret string
	err := db.QueryRow("SELECT value FROM config WHERE key = 'totp_secret'").Scan(&totpSecret)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	c.TOTPSecret = totpSecret

	// 加载 totp_enabled
	var totpEnabled int
	err = db.QueryRow("SELECT value FROM config WHERE key = 'totp_enabled'").Scan(&totpEnabled)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	c.TOTPEnabled = totpEnabled == 1

	// 加载 session_tokens (JSON 数组)
	var sessionTokensJSON string
	err = db.QueryRow("SELECT value FROM config WHERE key = 'session_tokens'").Scan(&sessionTokensJSON)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if sessionTokensJSON != "" {
		json.Unmarshal([]byte(sessionTokensJSON), &c.SessionTokens)
	} else {
		// 兼容旧版本：尝试加载旧的 session_token
		var oldToken string
		err = db.QueryRow("SELECT value FROM config WHERE key = 'session_token'").Scan(&oldToken)
		if err == nil && oldToken != "" {
			c.SessionTokens = []SessionTokenEntry{{Token: oldToken, CreatedAt: time.Now()}}
		}
	}

	// 加载 providers
	rows, err := db.Query("SELECT id, name, base_url, api_key, model, is_active, created_at, order_num, COALESCE(is_openai_format, 0), COALESCE(quota_block_enabled, 0), COALESCE(quota_block_threshold, 99) FROM providers ORDER BY order_num")
	if err != nil {
		return err
	}
	defer rows.Close()

	c.Providers = nil
	c.QuotaBlockEnabled = map[string]bool{}
	for rows.Next() {
		var p Provider
		var isActive int
		var isOpenAIFormat int
		var quotaBlock int
		var quotaThreshold int
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &isActive, &p.CreatedAt, &p.Order, &isOpenAIFormat, &quotaBlock, &quotaThreshold); err != nil {
			return err
		}
		p.IsActive = isActive == 1
		p.IsOpenAIFormat = isOpenAIFormat == 1
		p.QuotaBlockThreshold = quotaThreshold
		c.QuotaBlockEnabled[p.ID] = quotaBlock == 1
		c.Providers = append(c.Providers, p)
	}

	// 加载 server_keys
	rows, err = db.Query("SELECT id, key, remark, is_enabled, created_at, order_num, COALESCE(daily_req_limit, 0), COALESCE(total_req_limit, 0), COALESCE(daily_cost_limit, 0), COALESCE(total_cost_limit, 0) FROM server_keys ORDER BY order_num")
	if err != nil {
		return err
	}
	defer rows.Close()

	c.ServerKeys = nil
	for rows.Next() {
		var k ServerKey
		var isEnabled int
		if err := rows.Scan(&k.ID, &k.Key, &k.Remark, &isEnabled, &k.CreatedAt, &k.Order, &k.DailyReqLimit, &k.TotalReqLimit, &k.DailyCostLimit, &k.TotalCostLimit); err != nil {
			return err
		}
		k.IsEnabled = isEnabled == 1
		c.ServerKeys = append(c.ServerKeys, k)
	}

	// 加载 mappings（合并到对应 key）
	if c.ServerKeys == nil {
		c.ServerKeys = []ServerKey{}
	}
	for i := range c.ServerKeys {
		c.ServerKeys[i].Mappings = c.loadMappingsForKeyNoLock(c.ServerKeys[i].ID)
	}

	return nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.save()
}

func (c *Config) save() error {
	// 保存 totp_secret
	_, err := db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('totp_secret', ?)", c.TOTPSecret)
	if err != nil {
		return err
	}

	// 保存 totp_enabled
	totpEnabled := 0
	if c.TOTPEnabled {
		totpEnabled = 1
	}
	_, err = db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('totp_enabled', ?)", totpEnabled)
	if err != nil {
		return err
	}

	// 保存 session_tokens (JSON 数组)
	sessionTokensJSON, _ := json.Marshal(c.SessionTokens)
	_, err = db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES ('session_tokens', ?)", string(sessionTokensJSON))
	if err != nil {
		return err
	}

	// 删除并重新插入 providers
	_, err = db.Exec("DELETE FROM providers")
	if err != nil {
		return err
	}
	for _, p := range c.Providers {
		isActive := 0
		if p.IsActive {
			isActive = 1
		}
		isOpenAIFormat := 0
		if p.IsOpenAIFormat {
			isOpenAIFormat = 1
		}
		quotaBlock := 0
		if c.QuotaBlockEnabled[p.ID] {
			quotaBlock = 1
		}
		// QuotaBlockThreshold 由 DB DEFAULT 99 提供兜底：当 p.QuotaBlockThreshold==0
		// （即调用方未显式设值，例如旧 Provider 调用栈或测试构造），省略 INSERT 列以让
		// SQLite 触发 DEFAULT 99。不在 Go 层做 ==0 翻译，避免把"用户显式存 0"和"未设值"混在一起。
		if p.QuotaBlockThreshold == 0 {
			_, err = db.Exec("INSERT INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, quota_block_enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				p.ID, p.Name, p.BaseURL, p.APIKey, p.Model, isActive, p.CreatedAt, p.Order, isOpenAIFormat, quotaBlock)
		} else {
			_, err = db.Exec("INSERT INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, quota_block_enabled, quota_block_threshold) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				p.ID, p.Name, p.BaseURL, p.APIKey, p.Model, isActive, p.CreatedAt, p.Order, isOpenAIFormat, quotaBlock, p.QuotaBlockThreshold)
		}
		if err != nil {
			return err
		}
	}

	// 删除并重新插入 server_keys
	_, err = db.Exec("DELETE FROM server_keys")
	if err != nil {
		return err
	}
	for _, k := range c.ServerKeys {
		isEnabled := 0
		if k.IsEnabled {
			isEnabled = 1
		}
		_, err = db.Exec("INSERT INTO server_keys (id, key, remark, is_enabled, created_at, order_num, daily_req_limit, total_req_limit, daily_cost_limit, total_cost_limit) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			k.ID, k.Key, k.Remark, isEnabled, k.CreatedAt, k.Order, k.DailyReqLimit, k.TotalReqLimit, k.DailyCostLimit, k.TotalCostLimit)
		if err != nil {
			return err
		}
	}

	// 清理遗留的 active_provider 行（Task 10 移除字段后无引用）
	db.Exec("DELETE FROM config WHERE key = 'active_provider'")

	return nil
}

func GetConfig() *Config {
	return cfg
}

// GetQuotaBlockEnabled returns a snapshot of the per-provider quota
// block-enforcement flags (provider_id -> enabled). Used by the quota
// package at startup (loadBlockFlagsFromConfig) to hydrate its in-memory
// map. Returns nil if the config singleton has not been initialized.
//
// The returned map is a copy safe to iterate without holding the lock.
func GetQuotaBlockEnabled() map[string]bool {
	if cfg == nil {
		return nil
	}
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	out := make(map[string]bool, len(cfg.QuotaBlockEnabled))
	for k, v := range cfg.QuotaBlockEnabled {
		out[k] = v
	}
	return out
}

// IterateProviders invokes fn with each provider snapshot under the
// config's read lock. Iteration order is the slice's natural order
// (sorted by Order). The provider pointer passed to fn aliases the
// internal slice element — callers MUST NOT mutate the pointed-to
// Provider. Returns nil if the config singleton has not been initialized.
//
// Used by the quota package (eligibleProviders) to enumerate providers
// without exposing the unexported mutex.
func IterateProviders(fn func(p *Provider)) {
	if cfg == nil {
		return
	}
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	for i := range cfg.Providers {
		fn(&cfg.Providers[i])
	}
}

// GetFirstActiveProvider 返回第一个 IsActive=true 的 provider，用于 testProvider 等场景。
func (c *Config) GetFirstActiveProvider() *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].IsActive {
			return &c.Providers[i]
		}
	}
	if len(c.Providers) > 0 {
		return &c.Providers[0]
	}
	return nil
}

// GetProviderByID 根据ID获取提供商
func (c *Config) GetProviderByID(id string) *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}

func (c *Config) AddProvider(p Provider) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 自动分配序号（最大序号 + 1）
	maxOrder := 0
	for _, provider := range c.Providers {
		if provider.Order > maxOrder {
			maxOrder = provider.Order
		}
	}
	p.Order = maxOrder + 1

	c.Providers = append(c.Providers, p)

	// 同步初始化 QuotaBlockEnabled map 入口（默认值 false）。
	// 防御 nil-map：防止后续 quota.IsBlocked 或 web 切换 handler 在
	// 未初始化 map 上 panic / 读到 nil 零值；与 SetProviderQuotaBlockEnabled
	// 的 nil 安全模式保持一致。
	if c.QuotaBlockEnabled == nil {
		c.QuotaBlockEnabled = map[string]bool{}
	}
	c.QuotaBlockEnabled[p.ID] = false

	// 按序号排序
	c.sortProviders()

	return c.save()
}

func (c *Config) UpdateProvider(id string, p Provider) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			// 保留原有序号
			p.Order = c.Providers[i].Order
			c.Providers[i] = p
			c.sortProviders()
			return c.save()
		}
	}

	return nil
}

func (c *Config) DeleteProvider(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.Providers {
		if c.Providers[i].ID == id {
			deletedOrder := c.Providers[i].Order
			c.Providers = append(c.Providers[:i], c.Providers[i+1:]...)

			// 重新调整序号，保持连续
			for j := range c.Providers {
				if c.Providers[j].Order > deletedOrder {
					c.Providers[j].Order--
				}
			}

			// 清理 QuotaBlockEnabled map 中的孤儿入口，避免长时间运行下累积。
			// map 可能是 nil（外部代码误用），nil-map delete 在 Go 中是 no-op、安全的。
			delete(c.QuotaBlockEnabled, id)

			c.sortProviders()
			return c.save()
		}
	}

	return nil
}

// sortProviders 按序号排序提供商（内部使用，调用前需要加锁）
func (c *Config) sortProviders() {
	sort.Slice(c.Providers, func(i, j int) bool {
		return c.Providers[i].Order < c.Providers[j].Order
	})
}

// SetProviderQuotaBlockEnabled 更新某个 provider 的额度拦截开关：
//   - 同步更新内存中的 c.QuotaBlockEnabled[id]，供 quota.IsBlocked() O(1) 查询；
//   - 同步写入 DB 的 quota_block_enabled 列，重启后由 Load() 重建。
func (c *Config) SetProviderQuotaBlockEnabled(id string, enabled bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.QuotaBlockEnabled == nil {
		c.QuotaBlockEnabled = map[string]bool{}
	}
	c.QuotaBlockEnabled[id] = enabled
	_, err := db.Exec("UPDATE providers SET quota_block_enabled = ? WHERE id = ?", boolToInt(enabled), id)
	return err
}

// SetProviderQuotaBlockThreshold 更新某个 provider 的拦截阈值（1..100）。
//   - 同步更新内存中的 c.Providers[i].QuotaBlockThreshold（O(1)）；
//   - 同步写入 DB 的 quota_block_threshold 列，重启后由 Load() 重建。
// 校验由 web 层 handler 负责（1 ≤ threshold ≤ 100），setter 仅做透传。
func (c *Config) SetProviderQuotaBlockThreshold(id string, threshold int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			c.Providers[i].QuotaBlockThreshold = threshold
			break
		}
	}
	_, err := db.Exec("UPDATE providers SET quota_block_threshold = ? WHERE id = ?", threshold, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// generateServerKeyBody returns a fresh sk- + 16 chars [a-zA-Z0-9] string.
// Pure: no DB side effects. Used by both GenerateServerKey (which persists) and
// GenerateServerKeyString (which just hands the value back to the caller).
func (c *Config) generateServerKeyBody() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	keyStr := "sk-"
	for _, b := range bytes {
		keyStr += string(chars[int(b)%len(chars)])
	}
	return keyStr, nil
}

// GenerateServerKeyString returns a fresh server key value without touching the DB.
// Use this when the caller only wants a candidate key (e.g. the edit-modal
// "regenerate" button), not a persisted entry.
func (c *Config) GenerateServerKeyString() (string, error) {
	return c.generateServerKeyBody()
}

// GenerateServerKey 生成新的服务器密钥并添加到列表
func (c *Config) GenerateServerKey() (string, error) {
	keyStr, err := c.generateServerKeyBody()
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 查找最大序号
	maxOrder := 0
	for _, k := range c.ServerKeys {
		if k.Order > maxOrder {
			maxOrder = k.Order
		}
	}

	serverKey := ServerKey{
		ID:        uuid.New().String(),
		Key:       keyStr,
		Remark:    "",
		IsEnabled: true,
		CreatedAt: time.Now().Format(time.RFC3339),
		Order:     maxOrder + 1,
	}

	c.ServerKeys = append(c.ServerKeys, serverKey)

	if err := c.save(); err != nil {
		return "", err
	}

	return keyStr, nil
}

// GetServerKeys 获取所有服务器密钥
func (c *Config) GetServerKeys() []ServerKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ServerKeys
}

// ValidateServerKey 验证密钥是否有效，返回密钥ID和是否有效
func (c *Config) ValidateServerKey(keyStr string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, k := range c.ServerKeys {
		if k.Key == keyStr && k.IsEnabled {
			return k.ID, true
		}
	}
	return "", false
}

// GetServerKeyByID 获取服务器密钥（包含限额信息）
func (c *Config) GetServerKeyByID(id string) *ServerKey {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			return &c.ServerKeys[i]
		}
	}
	return nil
}

// AddServerKey 添加服务器密钥
func (c *Config) AddServerKey(key ServerKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 查找最大序号
	maxOrder := 0
	for _, k := range c.ServerKeys {
		if k.Order > maxOrder {
			maxOrder = k.Order
		}
	}

	key.ID = uuid.New().String()
	key.CreatedAt = time.Now().Format(time.RFC3339)
	key.Order = maxOrder + 1

	c.ServerKeys = append(c.ServerKeys, key)
	return c.save()
}

// UpdateServerKey 更新服务器密钥
func (c *Config) UpdateServerKey(id string, key ServerKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID != id {
			continue
		}
		// key.Key == "" 表示前端未传，沿用旧值；非空则必须校验且唯一。
		if key.Key == "" {
			key.Key = c.ServerKeys[i].Key
		} else {
			if err := validateServerKeyFormat(key.Key); err != nil {
				return err
			}
			for j := range c.ServerKeys {
				if j != i && c.ServerKeys[j].Key == key.Key {
					return ErrServerKeyDuplicate
				}
			}
		}
		// 保留原有序号和创建时间
		key.ID = id
		key.CreatedAt = c.ServerKeys[i].CreatedAt
		key.Order = c.ServerKeys[i].Order
		key.Mappings = c.ServerKeys[i].Mappings // 前端编辑表单不发 mappings，整体替换会清空内存里的映射
		c.ServerKeys[i] = key
		return c.save()
	}

	return nil
}

// DeleteServerKey 删除服务器密钥
func (c *Config) DeleteServerKey(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			deletedOrder := c.ServerKeys[i].Order
			c.ServerKeys = append(c.ServerKeys[:i], c.ServerKeys[i+1:]...)

			// 重新调整序号
			for j := range c.ServerKeys {
				if c.ServerKeys[j].Order > deletedOrder {
					c.ServerKeys[j].Order--
				}
			}

			return c.save()
		}
	}

	return nil
}

// GetTOTPSecret 获取 TOTP 密钥
func (c *Config) GetTOTPSecret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TOTPSecret
}

// IsTOTPEnabled 检查 TOTP 是否已启用
func (c *Config) IsTOTPEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TOTPEnabled
}

// SetTOTPSecret 设置 TOTP 密钥（首次设置时调用）
func (c *Config) SetTOTPSecret(secret string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPSecret = secret
	c.TOTPEnabled = false // 首次设置时未启用，需要验证后启用
	return c.save()
}

// EnableTOTP 启用 TOTP（验证成功后调用）
func (c *Config) EnableTOTP() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPEnabled = true
	return c.save()
}

// GetSessionTokens 获取所有会话 tokens
func (c *Config) GetSessionTokens() []SessionTokenEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionTokens
}

// ValidateSessionToken 验证会话 token 是否有效
func (c *Config) ValidateSessionToken(token string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, entry := range c.SessionTokens {
		if entry.Token == token {
			return true
		}
	}
	return false
}

// AddSessionToken 添加新的会话 token
func (c *Config) AddSessionToken(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionTokens = append(c.SessionTokens, SessionTokenEntry{
		Token:     token,
		CreatedAt: time.Now(),
	})
	return c.save()
}

// RemoveSessionToken 移除指定的会话 token
func (c *Config) RemoveSessionToken(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, entry := range c.SessionTokens {
		if entry.Token == token {
			c.SessionTokens = append(c.SessionTokens[:i], c.SessionTokens[i+1:]...)
			return c.save()
		}
	}
	return nil
}

// ClearAllSessionTokens 清除所有会话 token
func (c *Config) ClearAllSessionTokens() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionTokens = nil
	return c.save()
}

// ResetTOTP 重置 TOTP 数据（清除密钥和启用状态，但保留其他配置）
func (c *Config) ResetTOTP() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.TOTPSecret = ""
	c.TOTPEnabled = false
	return c.save()
}

// LoadMappingsForKey 返回指定 key 的所有 mappings。
// 公开 API：会加读锁。Load() 内部持有写锁时不能调此方法，请改用 loadMappingsForKeyNoLock。
func (c *Config) LoadMappingsForKey(keyID string) []ModelMapping {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadMappingsForKeyNoLock(keyID)
}

// loadMappingsForKeyNoLock 是 LoadMappingsForKey 的无锁实现，调用前需持有 c.mu 读或写锁。
func (c *Config) loadMappingsForKeyNoLock(keyID string) []ModelMapping {
	rows, err := db.Query("SELECT id, server_key_id, user_model, provider_id, provider_model, created_at FROM model_mappings WHERE server_key_id = ? ORDER BY created_at", keyID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []ModelMapping
	for rows.Next() {
		var m ModelMapping
		if err := rows.Scan(&m.ID, &m.ServerKeyID, &m.UserModel, &m.ProviderID, &m.ProviderModel, &m.CreatedAt); err != nil {
			return out
		}
		out = append(out, m)
	}
	return out
}

// GetMappingForRouting 查找 keyID+userModel 的映射，返回 (mapping, target_provider, error)
// 错误语义：
//   - 找不到映射 → "model not allowed for this key"
//   - provider 不存在 → "configured provider missing"
//   - provider IsActive=false → "model not supported (provider inactive)"
func (c *Config) GetMappingForRouting(keyID, userModel string) (*ModelMapping, *Provider, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var m ModelMapping
	err := db.QueryRow(
		"SELECT id, server_key_id, user_model, provider_id, provider_model, created_at FROM model_mappings WHERE server_key_id = ? AND user_model = ?",
		keyID, userModel,
	).Scan(&m.ID, &m.ServerKeyID, &m.UserModel, &m.ProviderID, &m.ProviderModel, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil, fmt.Errorf("model %q not allowed for this key", userModel)
	}
	if err != nil {
		return nil, nil, err
	}

	for i := range c.Providers {
		if c.Providers[i].ID == m.ProviderID {
			if !c.Providers[i].IsActive {
				return nil, nil, fmt.Errorf("model %q not supported (provider inactive)", userModel)
			}
			return &m, &c.Providers[i], nil
		}
	}
	return nil, nil, ErrConfiguredProviderMissing
}

// AddMapping 添加一条映射；UNIQUE 冲突返回 error
// 同时同步更新内存中 ServerKey.Mappings，保证 GetServerKeys 即时返回新数据。
func (c *Config) AddMapping(keyID string, m ModelMapping) (ModelMapping, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	m.ID = uuid.New().String()
	m.ServerKeyID = keyID
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().Format(time.RFC3339)
	}

	_, err := db.Exec(
		"INSERT INTO model_mappings (id, server_key_id, user_model, provider_id, provider_model, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		m.ID, m.ServerKeyID, m.UserModel, m.ProviderID, m.ProviderModel, m.CreatedAt,
	)
	if err != nil {
		return ModelMapping{}, err
	}

	if idx := c.findKeyIndex(keyID); idx >= 0 {
		c.ServerKeys[idx].Mappings = append(c.ServerKeys[idx].Mappings, m)
	}
	return m, nil
}

// UpdateMapping 更新一条映射，同时同步更新内存中对应的 ModelMapping。
func (c *Config) UpdateMapping(keyID, mappingID string, m ModelMapping) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	res, err := db.Exec(
		"UPDATE model_mappings SET user_model = ?, provider_id = ?, provider_model = ? WHERE id = ? AND server_key_id = ?",
		m.UserModel, m.ProviderID, m.ProviderModel, mappingID, keyID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mapping not found")
	}

	if idx := c.findKeyIndex(keyID); idx >= 0 {
		for j := range c.ServerKeys[idx].Mappings {
			if c.ServerKeys[idx].Mappings[j].ID == mappingID {
				c.ServerKeys[idx].Mappings[j].UserModel = m.UserModel
				c.ServerKeys[idx].Mappings[j].ProviderID = m.ProviderID
				c.ServerKeys[idx].Mappings[j].ProviderModel = m.ProviderModel
				break
			}
		}
	}
	return nil
}

// DeleteMapping 删除一条映射，同时从内存中对应 ServerKey.Mappings 移除。
func (c *Config) DeleteMapping(keyID, mappingID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := db.Exec("DELETE FROM model_mappings WHERE id = ? AND server_key_id = ?", mappingID, keyID)
	if err != nil {
		return err
	}

	if idx := c.findKeyIndex(keyID); idx >= 0 {
		mappings := c.ServerKeys[idx].Mappings
		for j := range mappings {
			if mappings[j].ID == mappingID {
				c.ServerKeys[idx].Mappings = append(mappings[:j], mappings[j+1:]...)
				break
			}
		}
	}
	return nil
}

// findKeyIndex 返回 ServerKey slice 中 ID 匹配的下标；找不到返回 -1。
// 调用前需持有 c.mu 写锁。
func (c *Config) findKeyIndex(id string) int {
	for i := range c.ServerKeys {
		if c.ServerKeys[i].ID == id {
			return i
		}
	}
	return -1
}

// GetActiveMappingsForKey 返回该 key 关联的、且目标 provider 处于 active 状态的映射。
// 与 GetMappingForRouting 行为一致：inactive provider 的映射对调用方不可见。
// 公开 API：会加读锁。返回 nil 时调用方应按空集合处理（建议 range 前做 len 检查）。
func (c *Config) GetActiveMappingsForKey(keyID string) []ModelMapping {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rows, err := db.Query(
		"SELECT m.id, m.server_key_id, m.user_model, m.provider_id, m.provider_model, m.created_at "+
			"FROM model_mappings m INNER JOIN providers p ON p.id = m.provider_id "+
			"WHERE m.server_key_id = ? AND p.is_active = 1 ORDER BY m.created_at",
		keyID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []ModelMapping
	for rows.Next() {
		var m ModelMapping
		if err := rows.Scan(&m.ID, &m.ServerKeyID, &m.UserModel, &m.ProviderID, &m.ProviderModel, &m.CreatedAt); err != nil {
			return out
		}
		out = append(out, m)
	}
	return out
}

// HasMappingsForProvider 返回指定 provider_id 是否被任意 mapping 引用
func (c *Config) HasMappingsForProvider(providerID string) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM model_mappings WHERE provider_id = ?", providerID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Shutdown 关闭数据库连接
func Shutdown() {
	if db != nil {
		db.Close()
	}
}
