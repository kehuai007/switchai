package config

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

type Provider struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	BaseURL   string   `json:"base_url"`
	APIKey    string   `json:"api_key"`
	Models    []string `json:"models"`
	IsActive  bool     `json:"is_active"`
	CreatedAt string   `json:"created_at"`
	Order     int      `json:"order"`
}

type Config struct {
	Providers      []Provider `json:"providers"`
	ActiveProvider string     `json:"active_provider"`
	mu             sync.RWMutex
}

var (
	cfg        *Config
	configFile = "providers.json"
)

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

	data, err := os.ReadFile(configFile)
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

	return os.WriteFile(configFile, data, 0644)
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
