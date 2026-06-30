// Package quota polls MiniMax token-plan usage for active providers and
// surfaces a per-provider, per-window (interval + weekly) snapshot in
// memory. Snapshots are exposed via Snapshot() and consumed by the web
// layer (provider-item-stats card) and the proxy layer (request gating).
package quota

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Snapshot holds the latest quota state for one provider's two windows.
type Snapshot struct {
	ProviderID string         `json:"-"`
	Interval   IntervalWindow `json:"interval"`
	Weekly     IntervalWindow `json:"weekly"`
}

// IntervalWindow describes one of the two quota windows (interval = ~5h,
// weekly = ~7d) as returned by the upstream token_plan endpoint.
type IntervalWindow struct {
	Enabled          bool      `json:"enabled"`
	RemainingPercent float64   `json:"remaining_percent,omitempty"`
	UsedPercent      float64   `json:"used_percent"`
	StartTime        time.Time `json:"start_time,omitempty"`
	EndTime          time.Time `json:"end_time,omitempty"`
	ResetInSec       int       `json:"reset_in_sec,omitempty"`
	ResetInHuman     string    `json:"reset_in_human,omitempty"`
	TotalCount       int64     `json:"total_count,omitempty"`
	UsageCount       int64     `json:"usage_count,omitempty"`
	Status           int       `json:"status,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	LastErrorAt      time.Time `json:"last_error_at,omitempty"`
	LastSuccessAt    time.Time `json:"last_success_at,omitempty"`
}

// BlockInfo is returned by IsBlocked when a window is over the threshold.
type BlockInfo struct {
	Window       string  `json:"window"`
	UsedPercent  float64 `json:"used_percent"`
	ResetInSec   int     `json:"reset_in_sec"`
	ResetInHuman string  `json:"reset_in_human"`
}

const (
	pollInterval   = 10 * time.Second
	requestTimeout = 5 * time.Second
	blockThreshold = 99.0
	upstreamHost   = "https://api.minimaxi.com"
	upstreamPath   = "/v1/token_plan/remains"
	generalModel   = "general"
)

// isQuotaHost returns true if baseURL's host is minimaxi.com or a subdomain.
func isQuotaHost(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Host)
	const suffix = "minimaxi.com"
	return host == suffix || strings.HasSuffix(host, "."+suffix)
}

// Package-level state. guarded by stateMu.
var (
	stateMu      sync.RWMutex
	snapshots    = map[string]*Snapshot{} // providerID -> latest
	blockEnabled = map[string]bool{}      // providerID -> toggle
	upstreamHTTP *http.Client             // initialized in Init()
)
