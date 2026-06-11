package ws

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	raw := "wss://api.openmind.com/api/core/ota/agent?api_key_id=abc123&api_key=om_live_secretvalue"
	got := redactURL(raw)

	for _, secret := range []string{"om_live_secretvalue", "abc123"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redactURL leaked secret %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "api_key=REDACTED") || !strings.Contains(got, "api_key_id=REDACTED") {
		t.Fatalf("redactURL did not redact credentials: %s", got)
	}
	if !strings.HasPrefix(got, "wss://api.openmind.com/api/core/ota/agent") {
		t.Fatalf("redactURL changed host/path: %s", got)
	}
}
