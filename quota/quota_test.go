package quota

import "testing"

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
