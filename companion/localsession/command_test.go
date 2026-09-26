package localsession

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseLocalSessionCommands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want Action
	}{
		{"state permits empty scope", `{"version":"1","action":"state","scope":""}`, ActionState},
		{"select harness", `{"version":"1","action":"select_harness","scope":"scope-1","harness":"codex"}`, ActionSelect},
		{"prepare persona", `{"version":"1","action":"prepare","scope":"scope-1","harness":"claude_code","persona_id":"persona-1"}`, ActionPrepare},
		{"configure bundle", `{"version":"1","action":"configure","scope":"scope-1","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":{"persona_id":"persona-1"}}`, ActionConfigure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(tc.body))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Action != tc.want {
				t.Fatalf("Parse() action = %q, want %q", got.Action, tc.want)
			}
		})
	}
}

func TestParseRejectsInvalidOrUnboundedRequests(t *testing.T) {
	t.Parallel()
	valid := `{"version":"1","action":"prepare","scope":"scope-1","harness":"codex","persona_id":"persona-1"}`
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"unknown key", []byte(`{"version":"1","action":"state","scope":"s","extra":true}`), ErrInvalidRequest},
		{"wrong version", []byte(`{"version":"2","action":"state","scope":"s"}`), ErrInvalidRequest},
		{"unknown harness", []byte(`{"version":"1","action":"select_harness","scope":"s","harness":"shell"}`), ErrInvalidRequest},
		{"empty prepare scope", []byte(`{"version":"1","action":"prepare","scope":"","harness":"codex","persona_id":"p"}`), ErrInvalidRequest},
		{"invalid persona id", []byte(`{"version":"1","action":"prepare","scope":"s","harness":"codex","persona_id":"../x"}`), ErrInvalidRequest},
		{"multiple values", append([]byte(valid), []byte(` {}`)...), ErrInvalidRequest},
		{"duplicate action", []byte(`{"version":"1","action":"state","action":"configure","scope":"s"}`), ErrInvalidRequest},
		{"duplicate pending id", []byte(`{"version":"1","action":"configure","scope":"s","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":{}}`), ErrInvalidRequest},
		{"duplicate bundle field", []byte(`{"version":"1","action":"configure","scope":"s","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":{"a":1,"a":2}}`), ErrInvalidRequest},
		{"invalid UTF-8 scope", append(append([]byte(`{"version":"1","action":"state","scope":"`), 0xff), []byte(`"}`)...), ErrInvalidRequest},
		{"oversized request", bytes.Repeat([]byte("x"), maxCommandWireBytes+1), ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseConfigureRejectsInvalidBundleEnvelope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"oversized bundle", `{"version":"1","action":"configure","scope":"scope-1","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":{"value":"` + strings.Repeat("x", maxBundleWireBytes+8192) + `"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.body))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Parse() error = %v, want %v", err, ErrInvalidBundle)
			}
		})
	}
}

func TestParseConfigureMapsMalformedFieldsToInvalidRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"invalid pending UUID", `{"version":"1","action":"configure","scope":"scope-1","pending_id":"not-a-uuid","bundle":{}}`},
		{"non-object bundle", `{"version":"1","action":"configure","scope":"scope-1","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":[]}`},
		{"null bundle", `{"version":"1","action":"configure","scope":"scope-1","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.body))
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Parse() error = %v, want %v", err, ErrInvalidRequest)
			}
		})
	}
}

func TestParseKeepsHTMLBundleWithinMacWireLimit(t *testing.T) {
	t.Parallel()
	bundle := `{"text":` + strings.Repeat(" ", 65536) + `"` + strings.Repeat("<", maxBundleWireBytes-32768) + `"}`
	body := `{"version":"1","action":"configure","scope":"scope-1","pending_id":"d2719c20-35ef-4f3a-b620-1e2bddbf851b","bundle":` + bundle + `}`
	command, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if command.Action != ActionConfigure {
		t.Fatalf("Parse() action = %q, want %q", command.Action, ActionConfigure)
	}
}
