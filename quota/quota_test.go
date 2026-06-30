package quota

import (
	"testing"
	"time"
)

// TestShutdown_BeforeInit_NoBlock verifies that calling Shutdown without
// ever calling Init returns promptly instead of blocking forever on the
// never-closed done channel.
func TestShutdown_BeforeInit_NoBlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		Shutdown()
		close(done)
	}()
	select {
	case <-done:
		// good: Shutdown returned without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked for >2s when Init was never called")
	}
}

func TestIsQuotaHost(t *testing.T) {
	cases := []struct {
		baseURL string
		want    bool
	}{
		{"https://api.minimaxi.com", true},
		{"https://api.minimaxi.com/v1", true},
		{"https://api.minimaxi.com/", true},
		{"https://www.minimaxi.com", true},
		// Plan typo on line 310: "https://MiniMax.com" with want=true doesn't match
		// minimaxi.com. The MiniMax consumer brand (MiniMax.com) is a different
		// domain from the API host (minimaxi.com). Corrected to false to align
		// with the plan's hardcoded suffix "minimaxi.com" and the constant
		// upstreamHost = "https://api.minimaxi.com".
		{"https://MiniMax.com", false},
		{"https://API.MINIMAXI.COM", true},
		{"https://evil.com", false},
		{"https://notminimaxi.com", false},
		{"", false},
		{"not-a-url", false},
		{"https://example.com?ref=minimaxi.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.baseURL, func(t *testing.T) {
			got := isQuotaHost(tc.baseURL)
			if got != tc.want {
				t.Errorf("isQuotaHost(%q) = %v, want %v", tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestIsBlocked_ToggleOff_NotBlocked(t *testing.T) {
	stateMu.Lock()
	snapshots["t1"] = &Snapshot{
		ProviderID: "t1",
		Interval:   IntervalWindow{Enabled: true, UsedPercent: 99.5},
	}
	blockEnabled["t1"] = false
	stateMu.Unlock()
	blocked, _ := IsBlocked("t1")
	if blocked {
		t.Error("toggle off should never block")
	}
}

func TestIsBlocked_ToggleOn_TripsInterval(t *testing.T) {
	now := time.Now().Add(time.Hour)
	stateMu.Lock()
	snapshots["t2"] = &Snapshot{
		ProviderID: "t2",
		Interval:   IntervalWindow{Enabled: true, UsedPercent: 99.5, EndTime: now},
		Weekly:     IntervalWindow{Enabled: true, UsedPercent: 50},
	}
	blockEnabled["t2"] = true
	stateMu.Unlock()
	blocked, info := IsBlocked("t2")
	if !blocked || info.Window != "interval" {
		t.Errorf("expected interval block, got %+v", info)
	}
}

func TestIsBlocked_ToggleOn_TripsWeekly(t *testing.T) {
	now := time.Now().Add(24 * time.Hour)
	stateMu.Lock()
	snapshots["t3"] = &Snapshot{
		ProviderID: "t3",
		Interval:   IntervalWindow{Enabled: true, UsedPercent: 50},
		Weekly:     IntervalWindow{Enabled: true, UsedPercent: 99.5, EndTime: now},
	}
	blockEnabled["t3"] = true
	stateMu.Unlock()
	blocked, info := IsBlocked("t3")
	if !blocked || info.Window != "weekly" {
		t.Errorf("expected weekly block, got %+v", info)
	}
}

func TestIsBlocked_AutoUnblockOnEndTime(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	stateMu.Lock()
	snapshots["t4"] = &Snapshot{
		ProviderID: "t4",
		Interval:   IntervalWindow{Enabled: true, UsedPercent: 100, EndTime: past},
	}
	blockEnabled["t4"] = true
	stateMu.Unlock()
	blocked, _ := IsBlocked("t4")
	if blocked {
		t.Error("should auto-unblock when end_time passed")
	}
}

func TestIsBlocked_BothUnderThreshold(t *testing.T) {
	stateMu.Lock()
	snapshots["t5"] = &Snapshot{
		ProviderID: "t5",
		Interval:   IntervalWindow{Enabled: true, UsedPercent: 50},
		Weekly:     IntervalWindow{Enabled: true, UsedPercent: 80},
	}
	blockEnabled["t5"] = true
	stateMu.Unlock()
	blocked, _ := IsBlocked("t5")
	if blocked {
		t.Error("should not block when both under 99")
	}
}

func TestSnapshot_ReturnsShallowCopy(t *testing.T) {
	stateMu.Lock()
	snapshots["t6"] = &Snapshot{ProviderID: "t6", Interval: IntervalWindow{Enabled: true, UsedPercent: 25}}
	stateMu.Unlock()
	view := Snapshots()
	if view["t6"] == nil || view["t6"].Interval.UsedPercent != 25 {
		t.Fatalf("snapshot missing: %+v", view["t6"])
	}
	// Mutating the copy should not affect internal state.
	view["t6"].Interval.UsedPercent = 999
	if getSnapshot("t6").Interval.UsedPercent == 999 {
		t.Error("snapshot should be read-only")
	}
}
