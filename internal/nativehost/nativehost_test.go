package nativehost

import (
	"encoding/json"
	"testing"
)

func TestAllowedOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"chrome-extension://" + nativeExtIDs[0] + "/", true},
		{"chrome-extension://" + nativeExtIDs[0], true},
		{"chrome-extension://unknownextid000000000000000000/", false},
		{"https://chrome-extension://" + nativeExtIDs[0] + "/", false},
		{"", false},
	} {
		if got := allowedOrigin(tc.origin); got != tc.want {
			t.Errorf("allowedOrigin(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

func TestNativeHostManifestAllowsAllIDs(t *testing.T) {
	data, err := nativeHostManifest("C:/path/absorb-agent-desktop.exe")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name           string   `json:"name"`
		Type           string   `json:"type"`
		Path           string   `json:"path"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != nativeHostName || m.Type != "stdio" || m.Path == "" {
		t.Fatalf("unexpected manifest fields: %+v", m)
	}
	if len(m.AllowedOrigins) != len(nativeExtIDs) {
		t.Fatalf("allowed_origins count = %d, want %d", len(m.AllowedOrigins), len(nativeExtIDs))
	}
	for i, id := range nativeExtIDs {
		if m.AllowedOrigins[i] != "chrome-extension://"+id+"/" {
			t.Errorf("allowed_origins[%d] = %q, want chrome-extension://%s/", i, m.AllowedOrigins[i], id)
		}
	}
}
