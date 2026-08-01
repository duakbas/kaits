package ws

import (
	"strings"
	"testing"
	"time"
)

// The three failure modes look identical from the phone — messages stop — and
// have completely different fixes. Misfiling one as another is how a day gets
// spent on the wrong problem, so the classifier gets a test.
func TestExplainClassifies(t *testing.T) {
	cases := []struct {
		reason string
		want   string // substring the explanation must contain, "" for silence
	}{
		{"websocket: close 1001 (going away): Child was killed", "SYSTEM KILLING THE APP"},
		{"read tcp 192.168.1.200:8080->192.168.1.72:33158: read: no route to host", "left the network"},
		{"read tcp 192.168.1.200:8080->192.168.1.72:32920: read: operation timed out", "stopped answering"},
		{"websocket: close 1000 (normal)", ""},
		{"unexpected EOF", ""},
	}
	for _, tc := range cases {
		got := classify(tc.reason)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%q: expected no explanation, got %q", tc.reason, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%q: explanation %q does not mention %q", tc.reason, got, tc.want)
		}
	}
	// A kill must not be confused with a network fault just because the string
	// happens to arrive over a network connection.
	kill := classify("websocket: close 1001 (going away): Child was killed")
	if strings.Contains(kill, "left the network") {
		t.Error("a kill was described as a network problem")
	}
	_ = time.Second
}
