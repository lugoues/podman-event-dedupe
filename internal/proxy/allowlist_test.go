package proxy

import "testing"

func TestAllowed(t *testing.T) {
	tests := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/_ping", true},
		{"HEAD", "/_ping", true},
		{"GET", "/v1.41/version", true},
		{"GET", "/events", true},
		{"GET", "/v1.41/events", true},
		{"GET", "/containers/json", true},
		{"GET", "/v1.41/containers/abc123/json", true},
		{"POST", "/v1.41/containers/abc123/start", false},
		{"DELETE", "/v1.41/containers/abc123", false},
		{"POST", "/containers/json", false},
		{"GET", "/containers/abc/logs", false},
		{"GET", "/v1.41/info", false},
		{"GET", "/v1.41/libpod/events", false},
		{"GET", "/vX/events", false},
		{"GET", "/v1.41/containers/a/b/json", false},
	}
	for _, tt := range tests {
		if got := Allowed(tt.method, tt.path); got != tt.want {
			t.Errorf("Allowed(%s %s) = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}
