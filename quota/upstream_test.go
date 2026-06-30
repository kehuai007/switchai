package quota

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseResponse_PicksGeneral(t *testing.T) {
	raw := `{
      "model_remains": [
        {
          "start_time": 1782784800000,
          "end_time": 1782802800000,
          "model_name": "general",
          "current_interval_total_count": 1000,
          "current_interval_usage_count": 190,
          "current_interval_status": 1,
          "current_interval_remaining_percent": 81,
          "current_weekly_total_count": 10000,
          "current_weekly_usage_count": 0,
          "current_weekly_status": 3,
          "current_weekly_remaining_percent": 100
        },
        {
          "start_time": 1782748800000,
          "end_time": 1782835200000,
          "model_name": "video",
          "current_interval_remaining_percent": 50,
          "current_weekly_remaining_percent": 60
        }
      ],
      "base_resp": {"status_code": 0, "status_msg": "success"}
    }`
	snap := parseResponse([]byte(raw))
	if snap == nil {
		t.Fatal("parseResponse returned nil")
	}
	if got := snap.Interval.RemainingPercent; got != 81 {
		t.Errorf("Interval.RemainingPercent = %v, want 81", got)
	}
	if got := snap.Interval.UsedPercent; got != 19 {
		t.Errorf("Interval.UsedPercent = %v, want 19", got)
	}
	if got := snap.Weekly.RemainingPercent; got != 100 {
		t.Errorf("Weekly.RemainingPercent = %v, want 100", got)
	}
	if got := snap.Weekly.UsedPercent; got != 0 {
		t.Errorf("Weekly.UsedPercent = %v, want 0", got)
	}
	// video entry should be ignored.
	if snap.Interval.RemainingPercent == 50 {
		t.Error("video entry leaked into Interval")
	}
}

func TestParseResponse_NoGeneral(t *testing.T) {
	raw := `{"model_remains":[{"model_name":"video"}],"base_resp":{"status_code":0}}`
	snap := parseResponse([]byte(raw))
	if snap != nil {
		t.Errorf("expected nil when general absent, got %+v", snap)
	}
}

func TestParseResponse_BaseRespError(t *testing.T) {
	raw := `{"model_remains":[{"model_name":"general","current_interval_remaining_percent":50}],"base_resp":{"status_code":1,"status_msg":"err"}}`
	snap := parseResponse([]byte(raw))
	if snap != nil {
		t.Errorf("expected nil on base_resp error, got %+v", snap)
	}
}

func TestPollOnce_FetchAndStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model_remains": []map[string]interface{}{{
				"model_name": "general",
				"current_interval_remaining_percent": 70,
				"current_weekly_remaining_percent": 80,
				"start_time": 1782784800000,
				"end_time": 1782802800000,
				"weekly_start_time": 1782662400000,
				"weekly_end_time": 1783267200000,
			}},
			"base_resp": map[string]interface{}{"status_code": 0},
		})
	}))
	defer srv.Close()

	// host passed as parameter — no package-level mutation.
	setSnapshot("p1", &Snapshot{ProviderID: "p1"})
	err := pollProvider("p1", "sk-test", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	snap := getSnapshot("p1")
	if snap == nil || snap.Interval.UsedPercent != 30 {
		t.Errorf("expected Interval.UsedPercent=30, got %+v", snap)
	}
}
