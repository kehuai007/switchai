package config

import (
	"reflect"
	"testing"
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