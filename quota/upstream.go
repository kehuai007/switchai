package quota

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// upstreamResponse mirrors the live-tested MiniMax token_plan/remains body.
// We only model the fields we consume; unknown fields are ignored.
type upstreamResponse struct {
	ModelRemains []upstreamWindow `json:"model_remains"`
	BaseResp     upstreamBase     `json:"base_resp"`
}

type upstreamBase struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

type upstreamWindow struct {
	StartTime                       int64  `json:"start_time"`
	EndTime                         int64  `json:"end_time"`
	ModelName                       string `json:"model_name"`
	CurrentIntervalTotalCount       int64  `json:"current_interval_total_count"`
	CurrentIntervalUsageCount       int64  `json:"current_interval_usage_count"`
	CurrentIntervalStatus           int    `json:"current_interval_status"`
	CurrentIntervalRemainingPercent float64 `json:"current_interval_remaining_percent"`
	CurrentWeeklyTotalCount         int64  `json:"current_weekly_total_count"`
	CurrentWeeklyUsageCount         int64  `json:"current_weekly_usage_count"`
	WeeklyStartTime                 int64  `json:"weekly_start_time"`
	WeeklyEndTime                   int64  `json:"weekly_end_time"`
	CurrentWeeklyStatus             int    `json:"current_weekly_status"`
	CurrentWeeklyRemainingPercent   float64 `json:"current_weekly_remaining_percent"`
}

// init initializes the upstream HTTP client once at package load.
// Proxy is explicitly disabled to prevent the local proxy from
// recursively proxying quota calls.
func init() {
	upstreamHTTP = &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
}

// parseResponse decodes one upstream response and returns a Snapshot
// populated from the "general" entry. Returns nil if general is absent
// or base_resp indicates an error.
func parseResponse(body []byte) *Snapshot {
	var resp upstreamResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.BaseResp.StatusCode != 0 {
		return nil
	}
	var general *upstreamWindow
	for i := range resp.ModelRemains {
		if resp.ModelRemains[i].ModelName == generalModel {
			general = &resp.ModelRemains[i]
			break
		}
	}
	if general == nil {
		return nil
	}
	now := time.Now()
	snap := &Snapshot{
		Interval: IntervalWindow{
			Enabled:          true,
			RemainingPercent: general.CurrentIntervalRemainingPercent,
			UsedPercent:      usedFromRemaining(general.CurrentIntervalRemainingPercent),
			StartTime:        msToTime(general.StartTime),
			EndTime:          msToTime(general.EndTime),
			ResetInSec:       secondsUntil(msToTime(general.EndTime)),
			ResetInHuman:     formatDuration(time.Until(msToTime(general.EndTime))),
			TotalCount:       general.CurrentIntervalTotalCount,
			UsageCount:       general.CurrentIntervalUsageCount,
			Status:           general.CurrentIntervalStatus,
			LastSuccessAt:    now,
		},
		Weekly: IntervalWindow{
			Enabled:          true,
			RemainingPercent: general.CurrentWeeklyRemainingPercent,
			UsedPercent:      usedFromRemaining(general.CurrentWeeklyRemainingPercent),
			StartTime:        msToTime(general.WeeklyStartTime),
			EndTime:          msToTime(general.WeeklyEndTime),
			ResetInSec:       secondsUntil(msToTime(general.WeeklyEndTime)),
			ResetInHuman:     formatDuration(time.Until(msToTime(general.WeeklyEndTime))),
			TotalCount:       general.CurrentWeeklyTotalCount,
			UsageCount:       general.CurrentWeeklyUsageCount,
			Status:           general.CurrentWeeklyStatus,
			LastSuccessAt:    now,
		},
	}
	return snap
}

func usedFromRemaining(remaining float64) float64 {
	used := 100 - remaining
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return math.Round(used*100) / 100
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.Unix(0, ms*int64(time.Millisecond))
}

func secondsUntil(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	d := time.Until(t)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// pollProvider fetches one provider's quota and updates its snapshot.
// apiKey is sent as Bearer token. host overrides the default upstreamHost
// (used by tests with httptest); pass "" to use the package default.
// Returns the HTTP error, if any.
func pollProvider(providerID, apiKey, host string) error {
	snap, err := pollProviderFull(providerID, apiKey, host)
	if err != nil {
		return err
	}
	setSnapshot(providerID, snap)
	return nil
}

// pollProviderFull fetches one provider's quota and returns the parsed
// Snapshot without touching package state. Caller decides whether to
// publish it via setSnapshot and/or persist it via RecordQuotaSnapshot.
// On 401, this returns a non-nil Snapshot with UsedPercent=100 + the
// lastSuccess timestamp so callers can decide how to surface the error.
func pollProviderFull(providerID, apiKey, host string) (*Snapshot, error) {
	if host == "" {
		host = upstreamHost
	}
	req, err := http.NewRequest("GET", host+upstreamPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "switchai-quota/1.0")

	resp, err := upstreamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// Force-block conservatively. Caller updates error state.
		// Safely read previous LastSuccessAt — getSnapshot may return nil
		// on first encounter, and we look at both windows.
		var lastSuccess time.Time
		if prev := getSnapshot(providerID); prev != nil {
			if !prev.Interval.LastSuccessAt.IsZero() {
				lastSuccess = prev.Interval.LastSuccessAt
			}
			if prev.Weekly.LastSuccessAt.After(lastSuccess) {
				lastSuccess = prev.Weekly.LastSuccessAt
			}
		}
		return &Snapshot{
			ProviderID: providerID,
			Interval: IntervalWindow{
				Enabled:       true,
				UsedPercent:   100,
				LastError:     "token 失效",
				LastErrorAt:   time.Now(),
				LastSuccessAt: lastSuccess,
			},
			Weekly: IntervalWindow{
				Enabled:       true,
				UsedPercent:   100,
				LastError:     "token 失效",
				LastErrorAt:   time.Now(),
				LastSuccessAt: lastSuccess,
			},
		}, fmt.Errorf("unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	snap := parseResponse(body)
	if snap == nil {
		return nil, fmt.Errorf("upstream response missing general model or base_resp error")
	}
	snap.ProviderID = providerID
	return snap, nil
}

// setSnapshot replaces the stored snapshot for providerID.
func setSnapshot(id string, snap *Snapshot) {
	stateMu.Lock()
	defer stateMu.Unlock()
	snapshots[id] = snap
}

// getSnapshot returns the stored snapshot for providerID, or nil if absent.
func getSnapshot(id string) *Snapshot {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return snapshots[id]
}
