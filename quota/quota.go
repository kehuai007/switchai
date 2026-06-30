// Package quota polls MiniMax token-plan usage for active providers and
// surfaces a per-provider, per-window (interval + weekly) snapshot in
// memory. Snapshots are exposed via Snapshot() and consumed by the web
// layer (provider-item-stats card) and the proxy layer (request gating).
package quota

import (
	"context"
	"fmt"
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

// IsBlocked returns whether the provider's quota should block upstream
// requests. Blocking requires (a) the user toggled enforcement ON for
// this provider, and (b) at least one window's UsedPercent is >= 99.
// If a window's EndTime has passed, that window is ignored (lazy reset).
func IsBlocked(providerID string) (bool, BlockInfo) {
	stateMu.RLock()
	snap := snapshots[providerID]
	enabled := blockEnabled[providerID]
	stateMu.RUnlock()

	if snap == nil || !enabled {
		return false, BlockInfo{}
	}

	now := time.Now()
	wins := []struct {
		name string
		w    IntervalWindow
	}{
		{"interval", snap.Interval},
		{"weekly", snap.Weekly},
	}
	for _, x := range wins {
		if !x.w.Enabled {
			continue
		}
		if !x.w.EndTime.IsZero() && now.After(x.w.EndTime) {
			continue
		}
		if x.w.UsedPercent >= blockThreshold {
			return true, BlockInfo{
				Window:       x.name,
				UsedPercent:  x.w.UsedPercent,
				ResetInSec:   x.w.ResetInSec,
				ResetInHuman: x.w.ResetInHuman,
			}
		}
	}
	return false, BlockInfo{}
}

// Snapshots returns a copy of all current snapshots keyed by providerID.
// Callers MUST treat the returned map and its values as read-only.
func Snapshots() map[string]*Snapshot {
	stateMu.RLock()
	defer stateMu.RUnlock()
	out := make(map[string]*Snapshot, len(snapshots))
	for k, v := range snapshots {
		// Shallow copy of struct fields (no pointers inside).
		c := *v
		out[k] = &c
	}
	return out
}

// SetBlockEnabled updates the in-memory enforcement flag. The web layer
// also persists this via config.SetProviderQuotaBlockEnabled.
func SetBlockEnabled(providerID string, enabled bool) {
	stateMu.Lock()
	defer stateMu.Unlock()
	blockEnabled[providerID] = enabled
}

// BlockEnabledFlags returns the current enforcement flags for all known
// providers. Used by web.getProviders to populate per-provider fields.
func BlockEnabledFlags() map[string]bool {
	stateMu.RLock()
	defer stateMu.RUnlock()
	out := make(map[string]bool, len(blockEnabled))
	for k, v := range blockEnabled {
		out[k] = v
	}
	return out
}

// Init starts the 10s polling loop. Returns an error if the package
// cannot start; call Shutdown to stop.
func Init(ctx context.Context) error {
	loadBlockFlagsFromConfig()
	stateMu.Lock()
	started = true
	stateMu.Unlock()
	go runLoop(ctx)
	return nil
}

// Shutdown signals the polling loop to exit and waits up to 5s for
// in-flight polls to drain. Safe to call before Init (no-op).
func Shutdown() {
	cancelOnce.Do(func() {
		stateMu.RLock()
		c := cancel
		stateMu.RUnlock()
		if c != nil {
			c()
		}
	})
	stateMu.RLock()
	wasStarted := started
	stateMu.RUnlock()
	if !wasStarted {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

var (
	cancel     context.CancelFunc
	cancelOnce sync.Once
	done       = make(chan struct{})
	started    bool // set true when Init() launches the polling goroutine
)

// runLoop is the ticker goroutine.
func runLoop(parent context.Context) {
	ctx, c := context.WithCancel(parent)
	stateMu.Lock()
	cancel = c
	stateMu.Unlock()
	defer close(done)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Initial sweep so the UI has data on first render.
	safePollOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safePollOnce()
		}
	}
}

func safePollOnce() {
	defer func() {
		if r := recover(); r != nil {
			// log via fmt since logger may not be initialized in tests
			fmt.Printf("quota: pollOnce panic: %v\n", r)
		}
	}()
	pollOnce()
}

// pollOnce iterates all known providers and polls those eligible.
// The full implementation references config.GetConfig() — stubbed here
// because Task 6 wires the integration.
func pollOnce() {
	providers := eligibleProviders()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(id, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := pollProvider(id, key, upstreamHost); err != nil {
				markError(id, err.Error())
			}
		}(p.id, p.key)
	}
	wg.Wait()
}

// markError records an error on the existing snapshot without changing
// its window data.
func markError(id, msg string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	snap := snapshots[id]
	if snap == nil {
		snap = &Snapshot{ProviderID: id}
		snapshots[id] = snap
	}
	snap.Interval.LastError = msg
	snap.Interval.LastErrorAt = time.Now()
	snap.Weekly.LastError = msg
	snap.Weekly.LastErrorAt = time.Now()
}

// loadBlockFlagsFromConfig hydrates blockEnabled from the config DB.
// Wired in Task 6.
func loadBlockFlagsFromConfig() {
	// Wired in Task 6. Until then, no-op.
	// config.GetQuotaBlockEnabled() will be called here once available.
}

// eligibleProvider is the minimal view of a provider needed by the poller.
type eligibleProvider struct {
	id  string
	key string
}

// eligibleProviders returns the providers we should poll. Wired in Task 6
// via config.GetConfig().Providers filtered by isQuotaHost / IsActive.
func eligibleProviders() []eligibleProvider {
	// Task 5+6 will wire to config.GetConfig().Providers
	return nil
}
