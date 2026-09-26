package desktopbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

func TestParseAcceptsOnlyFiniteInstallationRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want Request
	}{
		{name: "status", raw: `{"id":1,"action":"status"}`, want: Request{ID: 1, Action: ActionStatus}},
		{name: "enroll", raw: `{"id":2,"action":"enroll","ticket":"ticket"}`, want: Request{ID: 2, Action: ActionEnroll, Ticket: "ticket"}},
		{name: "attach", raw: `{"id":3,"action":"attach","ticket":"ticket"}`, want: Request{ID: 3, Action: ActionAttach, Ticket: "ticket"}},
		{name: "revoke", raw: `{"id":4,"action":"revoke"}`, want: Request{ID: 4, Action: ActionRevoke}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(tc.raw))
			if err != nil || got != tc.want {
				t.Fatalf("Parse() = %#v, %v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestParseRejectsMalformedOversizedAndExtraFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty"},
		{name: "invalid JSON", raw: `{"id":`},
		{name: "zero ID", raw: `{"id":0,"action":"status"}`},
		{name: "unsafe ID", raw: `{"id":9007199254740992,"action":"status"}`},
		{name: "unknown action", raw: `{"id":1,"action":"execute"}`},
		{name: "extra field", raw: `{"id":1,"action":"status","ticket":"secret"}`},
		{name: "duplicate ID", raw: `{"id":1,"id":2,"action":"revoke"}`},
		{name: "duplicate action", raw: `{"id":1,"action":"status","action":"revoke"}`},
		{name: "missing ticket", raw: `{"id":1,"action":"enroll"}`},
		{name: "unexpected ticket", raw: `{"id":1,"action":"revoke","ticket":"secret"}`},
		{name: "empty ticket", raw: `{"id":1,"action":"attach","ticket":" "}`},
		{name: "wrong field type", raw: `{"id":"1","action":"status"}`},
		{name: "trailing value", raw: `{"id":1,"action":"status"} {}`},
		{name: "oversized", raw: `{"id":1,"action":"status","padding":"` + strings.Repeat("x", MaxRequestBytes) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(tc.raw)); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Parse() error = %v", err)
			}
		})
	}
}

func TestParseRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	raw := append([]byte(`{"id":1,"action":"status","extra":"`), 0xff)
	raw = append(raw, []byte(`"}`)...)
	if _, err := Parse(raw); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Parse() invalid UTF-8 error = %v", err)
	}
}

func TestNewRestrictsAppOrigin(t *testing.T) {
	t.Parallel()
	if _, err := New(&serviceStub{}, "https://attacker.example"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := New(nil, "https://my.personastack.ai"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("New(nil) error = %v", err)
	}
	if _, err := New(&serviceStub{}, "https://my.personastack.ai"); err != nil {
		t.Fatalf("New(valid origin) error = %v", err)
	}
}

func TestProcessorDispatchesOnlyFiniteActionsAndRedactsStatus(t *testing.T) {
	t.Parallel()
	service := &serviceStub{status: installation.Status{Enrolled: true, CredentialValid: true, RelayActive: true}}
	processor, err := New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		request Request
		call    string
	}{
		{request: Request{ID: 1, Action: ActionStatus}, call: "status"},
		{request: Request{ID: 2, Action: ActionEnroll, Ticket: "ticket"}, call: "enroll"},
		{request: Request{ID: 3, Action: ActionAttach, Ticket: "ticket"}, call: "attach"},
		{request: Request{ID: 4, Action: ActionRevoke}, call: "revoke"},
	}
	for _, tc := range cases {
		response := processor.Handle(context.Background(), tc.request)
		if !response.OK || response.ID != tc.request.ID || service.calls[len(service.calls)-1] != tc.call {
			t.Fatalf("Handle(%s) = %#v, calls %v", tc.request.Action, response, service.calls)
		}
		if tc.call == "enroll" && service.operatingSystem != apicontract.DesktopControlOperatingSystemLinux {
			t.Fatalf("enrollment platform = %q", service.operatingSystem)
		}
	}
	if service.origin != "https://my.personastack.ai" {
		t.Fatalf("service origin = %q", service.origin)
	}
	encoded, err := json.Marshal(processor.Handle(context.Background(), Request{ID: 5, Action: ActionStatus}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "credential") && !strings.Contains(string(encoded), "credential_valid") {
		t.Fatalf("response disclosed credential field: %s", encoded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	status, ok := decoded["status"].(map[string]any)
	if !ok || status["enrolled"] != true || status["credential_valid"] != true || status["relay_active"] != true {
		t.Fatalf("status JSON = %#v", decoded["status"])
	}
}

func TestProcessorMapsErrorsToFiniteCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{name: "invalid request", err: desktopcontrol.ErrInvalidRequest, want: ErrorInvalidRequest},
		{name: "rejected", err: desktopcontrol.ErrRejected, want: ErrorRejected},
		{name: "other", err: errors.New("private API body"), want: ErrorUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := &serviceStub{errors: map[string]error{"revoke": tc.err}}
			processor, err := New(service, "https://my.personastack.ai")
			if err != nil {
				t.Fatal(err)
			}
			response := processor.Handle(context.Background(), Request{ID: 7, Action: ActionRevoke})
			if response.OK || response.Error != tc.want || response.ID != 7 {
				t.Fatalf("response = %#v, want error %q", response, tc.want)
			}
		})
	}
}

type serviceStub struct {
	calls           []string
	origin          string
	operatingSystem apicontract.DesktopControlOperatingSystem
	status          installation.Status
	errors          map[string]error
}

func (s *serviceStub) record(action, origin string) error {
	s.calls = append(s.calls, action)
	s.origin = origin
	return s.errors[action]
}

func (s *serviceStub) Enroll(_ context.Context, _ string, operatingSystem apicontract.DesktopControlOperatingSystem) error {
	s.calls = append(s.calls, "enroll")
	s.operatingSystem = operatingSystem
	return s.errors["enroll"]
}

func (s *serviceStub) Attach(_ context.Context, origin, _ string) error {
	return s.record("attach", origin)
}

func (s *serviceStub) Status(_ context.Context, origin string) (installation.Status, error) {
	if err := s.record("status", origin); err != nil {
		return installation.Status{}, err
	}
	return s.status, nil
}

func (s *serviceStub) Revoke(_ context.Context, origin string) error {
	return s.record("revoke", origin)
}

func TestErrorCodeMapsMissingAndKeyringErrors(t *testing.T) {
	t.Parallel()
	if got := errorCode(credentialstore.ErrCredentialMissing); got != ErrorNotEnrolled {
		t.Fatalf("missing credential code = %q", got)
	}
	if got := errorCode(credentialstore.ErrUnavailable); got != ErrorKeyringUnavailable {
		t.Fatalf("keyring error code = %q", got)
	}
}
