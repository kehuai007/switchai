package config

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"sort"
	"switchai/appdata"
	"sync"
	"time"

	"github.com/google/uuid"
)

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
}

type ServerKey struct {
	ID        string `json:"id"`        // 密钥ID
	Key       string `json:"key"`        // 密钥值 sk-xxxx
	Remark    string `json:"remark"`     // 备注
	IsEnabled bool   `json:"is_enabled"` // 是否启用
	CreatedAt string `json:"created_at"` // 创建时间
	Order     int    `json:"order"`      // 排序序号
}

type Config struct {
	Providers     []Provider  `json:"providers"`
	ServerKeys    []ServerKey `json:"server_keys"` // 服务器密钥列表
	ActiveProvider string     `json:"active_provider"`
	TOTPSecret    string     `json:"totp_secret"`  // TOTP 2FA 密钥
	TOTPEnabled   bool       `json:"totp_enabled"` // 是否已启用 2FA
	SessionToken  string     `json:"session_token"` // 持久化的会话 token
	SessionExpiry int64      `json:"session_expiry"` // 会话过期时间戳
	mu           sync.RWMutex
}

var cfg *Config

// getConfigFile 返回配置文件路径
func getConfigFile() string {
	return appdata.GetConfigPath("providers.json")
}

func Init() error {
	cfg = &Config{
		Providers: []Provider{},
	}

	// 尝试加载配置文件
	if err := cfg.Load(); err != nil {
		// 如果文件不存在，创建默认配置
		if os.IsNotExist(err) {
			return cfg.Save()
		}
		return err
	}

	return nil
}

func (c *Config) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(getConfigFile())
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, c); err != nil {
		return err
	}

	// 加载后按序号排序
	c.sortProviders()
	return nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.save()
}

func (c *Config) save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(getConfigFile(), data, 0644)
}

func GetConfig() *Config {
	return cfg
}

func (c *Config) GetActiveProvider() *Provider {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i := range c.Providers {
		if c.Providers[i].ID == c.ActiveProvider {
			return &c.Providers[i]
		}
	}

	// 如果没有活跃的提供商，返回第一个
	if len(c.Providers) > 0 {
		return &c.Providers[0]
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

	// 如果是第一个提供商，设置为活跃
	if len(c.Providers) == 1 {
		c.ActiveProvider = p.ID
	}

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

			// 如果删除的是活跃提供商，切换到第一个
			if c.ActiveProvider == id && len(c.Providers) > 0 {
				c.ActiveProvider = c.Providers[0].ID
			}

			c.sortProviders()
			return c.save()
		}
	}

	return nil
}

func (c *Config) SetActiveProvider(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ActiveProvider = id
	return c.save()
}

// sortProviders 按序号排序提供商（内部使用，调用前需要加锁）
func (c *Config) sortProviders() {
	sort.Slice(c.Providers, func(i, j int) bool {
		return c.Providers[i].Order < c.Providers[j].Order
	})
}

// GenerateServerKey 生成新的服务器密钥并添加到列表
func (c *Config) GenerateServerKey() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 生成 sk- 开头 + 16位随机字符
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	keyStr := "sk-"
	for _, b := range bytes {
		keyStr += string(chars[int(b)%len(chars)])
	}

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
		if c.ServerKeys[i].ID == id {
			// 保留原有序号和创建时间
			key.ID = id
			key.CreatedAt = c.ServerKeys[i].CreatedAt
			key.Order = c.ServerKeys[i].Order
			key.Key = c.ServerKeys[i].Key // 不允许修改密钥值
			c.ServerKeys[i] = key
			return c.save()
		}
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

// GetServerKeyByID 根据ID获取密钥
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

// GetSessionToken 获取会话 token
func (c *Config) GetSessionToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionToken
}

// GetSessionExpiry 获取会话过期时间
func (c *Config) GetSessionExpiry() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SessionExpiry
}

// SetSessionToken 设置会话 token（带 24 小时过期）
func (c *Config) SetSessionToken(token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionToken = token
	c.SessionExpiry = time.Now().Add(24 * time.Hour).Unix()
	return c.save()
}

// ClearSessionToken 清除会话 token
func (c *Config) ClearSessionToken() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.SessionToken = ""
	c.SessionExpiry = 0
	return c.save()
}
