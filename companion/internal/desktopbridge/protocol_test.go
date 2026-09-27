package desktopbridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/omarchy-desktop/companion/localsession"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

func TestParseAcceptsHostedDesktopControlCommands(t *testing.T) {
	t.Parallel()
	ticket := strings.Repeat("A", 43)
	cases := []struct {
		name string
		raw  string
		want Request
	}{
		{name: "sync", raw: `{"id":1,"version":"1","action":"sync","scope":"workspace:request"}`, want: Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:request"}},
		{name: "empty state scope", raw: `{"id":2,"version":"1","action":"state","scope":""}`, want: Request{ID: 2, Version: "1", Action: ActionState}},
		{name: "tray pause", raw: `{"id":3,"version":"1","action":"pause","scope":"` + LifecycleScope + `"}`, want: Request{ID: 3, Version: "1", Action: ActionPause, Scope: LifecycleScope}},
		{name: "tray resume", raw: `{"id":4,"version":"1","action":"resume","scope":"` + LifecycleScope + `"}`, want: Request{ID: 4, Version: "1", Action: ActionResume, Scope: LifecycleScope}},
		{name: "tray repair", raw: `{"id":8,"version":"1","action":"repair","scope":"` + LifecycleScope + `"}`, want: Request{ID: 8, Version: "1", Action: ActionRepair, Scope: LifecycleScope}},
		{name: "tray disconnect", raw: `{"id":6,"version":"1","action":"disconnect","scope":"` + LifecycleScope + `"}`, want: Request{ID: 6, Version: "1", Action: ActionDisconnect, Scope: LifecycleScope}},
		{name: "tray state without workspace sync", raw: `{"id":5,"version":"1","action":"state","scope":"` + LifecycleScope + `"}`, want: Request{ID: 5, Version: "1", Action: ActionState, Scope: LifecycleScope}},
		{name: "prepare", raw: `{"id":7,"version":"1","action":"prepare","scope":"workspace:request","enrollment_ticket":"` + ticket + `"}`, want: Request{ID: 7, Version: "1", Action: ActionPrepare, Scope: "workspace:request", EnrollmentTicket: ticket}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse([]byte(tc.raw))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Parse() = %#v, %v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestLifecycleScopeDoesNotRequireOrReplaceWorkspaceSync(t *testing.T) {
	t.Parallel()
	active := true
	lifecycle := &controlRuntimeStub{state: installation.LocalState{RelayPaused: true, RelayActive: &active}}
	processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	state := processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionState, Scope: LifecycleScope})
	if !state.OK || state.Result == nil || lifecycle.lifecycleStateCalls != 1 || lifecycle.stateCalls != 0 {
		t.Fatalf("tray state without sync = %#v local calls=%d lifecycle calls=%d", state, lifecycle.stateCalls, lifecycle.lifecycleStateCalls)
	}
	processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	paused := processor.Handle(context.Background(), Request{ID: 3, Version: "1", Action: ActionPause, Scope: LifecycleScope})
	wrongScopeResume := processor.Handle(context.Background(), Request{ID: 6, Version: "1", Action: ActionResume, Scope: "workspace:a"})
	workspaceState := processor.Handle(context.Background(), Request{ID: 4, Version: "1", Action: ActionState, Scope: "workspace:a"})
	if !paused.OK || wrongScopeResume.Error != ErrorInvalidRequest || !workspaceState.OK || lifecycle.stateCalls != 2 || lifecycle.lifecycleStateCalls != 1 {
		t.Fatalf("tray pause or scope fence: pause=%#v wrong-scope resume=%#v state=%#v local calls=%d lifecycle calls=%d", paused, wrongScopeResume, workspaceState, lifecycle.stateCalls, lifecycle.lifecycleStateCalls)
	}
}

func TestWorkspaceScopeChangeDoesNotCancelTrayLifecycleAction(t *testing.T) {
	t.Parallel()
	lifecycle := &controlRuntimeStub{
		state:        installation.LocalState{RelayPaused: true},
		pauseStarted: make(chan struct{}),
		pauseRelease: make(chan struct{}),
	}
	processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	result := make(chan Response, 1)
	go func() {
		result <- processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionPause, Scope: LifecycleScope})
	}()
	<-lifecycle.pauseStarted
	processor.Handle(context.Background(), Request{ID: 3, Version: "1", Action: ActionSync, Scope: "workspace:b"})
	close(lifecycle.pauseRelease)
	if response := <-result; !response.OK || response.Result == nil || lifecycle.stateCalls != 1 {
		t.Fatalf("tray pause after workspace change = %#v calls=%d", response, lifecycle.stateCalls)
	}
}

func TestParseAcceptsOnlyScopedLocalSessionCommands(t *testing.T) {
	t.Parallel()
	raw := `{"id":4,"version":"1","action":"local_session","scope":"workspace:a","local_session":{"version":"1","action":"state","scope":"workspace:a"}}`
	request, err := Parse([]byte(raw))
	if err != nil || request.Action != ActionLocalSession || string(request.LocalSession) != `{"version":"1","action":"state","scope":"workspace:a"}` {
		t.Fatalf("Parse() local session = %#v, %v", request, err)
	}
	for _, input := range []string{
		`{"id":4,"version":"1","action":"local_session","scope":"workspace:a","local_session":{"version":"1","action":"state","scope":"workspace:b"}}`,
		`{"id":4,"version":"1","action":"local_session","scope":"workspace:a","local_session":{"version":"1","action":"state","scope":"workspace:a","extra":true}}`,
		`{"id":4,"version":"1","action":"local_session","scope":"workspace:a","local_session":{"version":"1","action":"configure","scope":"workspace:a","pending_id":"x","bundle":{}}}`,
	} {
		if _, err := Parse([]byte(input)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Parse() accepted invalid local session command %s: %v", input, err)
		}
	}
}

func TestParseRejectsMalformedAndCredentialBearingCommands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty"},
		{name: "invalid JSON", raw: `{"id":`},
		{name: "zero ID", raw: `{"id":0,"version":"1","action":"sync","scope":""}`},
		{name: "unsafe ID", raw: `{"id":9007199254740992,"version":"1","action":"sync","scope":""}`},
		{name: "wrong version", raw: `{"id":1,"version":"2","action":"sync","scope":""}`},
		{name: "unknown action", raw: `{"id":1,"version":"1","action":"execute","scope":""}`},
		{name: "reserved lifecycle scope cannot sync a workspace", raw: `{"id":1,"version":"1","action":"sync","scope":"` + LifecycleScope + `"}`},
		{name: "pause must use lifecycle scope", raw: `{"id":1,"version":"1","action":"pause","scope":"workspace:a"}`},
		{name: "resume must use lifecycle scope", raw: `{"id":1,"version":"1","action":"resume","scope":"workspace:a"}`},
		{name: "disconnect must use lifecycle scope", raw: `{"id":1,"version":"1","action":"disconnect","scope":"workspace:a"}`},
		{name: "extra credential field", raw: `{"id":1,"version":"1","action":"state","scope":"","machine_credential":"secret"}`},
		{name: "duplicate ID", raw: `{"id":1,"id":2,"version":"1","action":"sync","scope":""}`},
		{name: "missing scope", raw: `{"id":1,"version":"1","action":"state"}`},
		{name: "unexpected ticket", raw: `{"id":1,"version":"1","action":"state","scope":"","enrollment_ticket":"secret"}`},
		{name: "short ticket", raw: `{"id":1,"version":"1","action":"prepare","scope":"x","enrollment_ticket":"ticket"}`},
		{name: "empty prepare scope", raw: `{"id":1,"version":"1","action":"prepare","scope":"","enrollment_ticket":"` + strings.Repeat("A", 43) + `"}`},
		{name: "scope whitespace", raw: `{"id":1,"version":"1","action":"sync","scope":" x"}`},
		{name: "scope exceeds UTF-8 byte limit", raw: `{"id":1,"version":"1","action":"sync","scope":"` + strings.Repeat("é", 257) + `"}`},
		{name: "wrong field type", raw: `{"id":"1","version":"1","action":"sync","scope":""}`},
		{name: "trailing value", raw: `{"id":1,"version":"1","action":"sync","scope":""} {}`},
		{name: "oversized", raw: `{"id":1,"version":"1","action":"sync","scope":"` + strings.Repeat("x", MaxRequestBytes) + `"}`},
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
	raw := append([]byte(`{"id":1,"version":"1","action":"sync","scope":"`), 0xff)
	raw = append(raw, []byte(`"}`)...)
	if _, err := Parse(raw); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Parse() invalid UTF-8 error = %v", err)
	}
}

func TestProcessorFencesStateBySynchronizedScope(t *testing.T) {
	t.Parallel()
	service := &serviceStub{state: installation.LocalState{RelayPaused: true}}
	processor, err := New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	state := processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionState})
	if state.OK || state.Error != ErrorInvalidRequest || service.calls != 0 {
		t.Fatalf("state before sync = %#v, service calls %d", state, service.calls)
	}
	processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionSync, Scope: ""})
	emptyPrepare := processor.Handle(context.Background(), Request{ID: 3, Version: "1", Action: ActionPrepare, EnrollmentTicket: strings.Repeat("A", 43)})
	if emptyPrepare.OK || emptyPrepare.Error != ErrorInvalidRequest || service.calls != 0 {
		t.Fatalf("prepare with empty synchronized scope = %#v, service calls %d", emptyPrepare, service.calls)
	}
	syncResult := processor.Handle(context.Background(), Request{ID: 4, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	if !syncResult.OK || syncResult.Result != nil {
		t.Fatalf("sync = %#v", syncResult)
	}
	wrongScope := processor.Handle(context.Background(), Request{ID: 5, Version: "1", Action: ActionState, Scope: "workspace:b"})
	if wrongScope.OK || service.calls != 0 {
		t.Fatalf("wrong-scope state = %#v, service calls %d", wrongScope, service.calls)
	}
	result := processor.Handle(context.Background(), Request{ID: 6, Version: "1", Action: ActionState, Scope: "workspace:a"})
	if !result.OK || result.Result == nil || result.Result.RuntimeAvailable == nil || *result.Result.RuntimeAvailable || service.calls != 1 || service.origin != "https://my.personastack.ai" {
		t.Fatalf("state = %#v, service calls %d", result, service.calls)
	}
}

func TestProcessorReadsTrayLifecycleStateWithoutHostedWorkspaceSync(t *testing.T) {
	t.Parallel()
	active := true
	lifecycle := &controlRuntimeStub{state: installation.LocalState{RelayActive: &active}}
	processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	result := processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionState, Scope: LifecycleScope})
	if !result.OK || result.Result == nil || result.Result.RelayActive == nil || !*result.Result.RelayActive || lifecycle.lifecycleStateCalls != 1 || lifecycle.stateCalls != 0 {
		t.Fatalf("tray lifecycle state = %#v, state calls=%d lifecycle calls=%d", result, lifecycle.stateCalls, lifecycle.lifecycleStateCalls)
	}
}

func TestProcessorPrepareReportsUnimplementedRuntimeWithoutEnrollment(t *testing.T) {
	t.Parallel()
	service := &serviceStub{state: installation.LocalState{RelayPaused: true}}
	processor, err := New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	result := processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionPrepare, Scope: "workspace:a", EnrollmentTicket: strings.Repeat("A", 43)})
	if !result.OK || result.Result == nil || result.Result.OperatingSystem != "linux" || result.Result.RuntimeAvailable == nil || *result.Result.RuntimeAvailable || result.Result.CuaReady || result.Result.NativeExecutorReady || result.Result.GatewayConnected {
		t.Fatalf("prepare reported unsupported readiness: %#v", result)
	}
	if service.calls != 1 {
		t.Fatalf("prepare made %d local-state reads, want no enrollment side effect", service.calls)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ticket") || strings.Contains(string(encoded), "credential") {
		t.Fatalf("prepare response disclosed sensitive field: %s", encoded)
	}
}

func TestProcessorPrepareUsesIntegratedRuntimeReadiness(t *testing.T) {
	t.Parallel()
	installationID := "install_01"
	active := true
	lifecycle := &controlRuntimeStub{state: installation.LocalState{
		InstallationID: &installationID, CuaReady: true, NativeExecutorReady: true,
		GatewayConnected: true, RelayActive: &active,
	}}
	processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	result := processor.Handle(context.Background(), Request{
		ID: 2, Version: "1", Action: ActionPrepare, Scope: "workspace:a", EnrollmentTicket: strings.Repeat("A", 43),
	})
	if !result.OK || result.Result == nil || result.Result.RuntimeAvailable == nil || !*result.Result.RuntimeAvailable ||
		result.Result.InstallationID == nil || *result.Result.InstallationID != installationID || !result.Result.CuaReady ||
		!result.Result.NativeExecutorReady || !result.Result.GatewayConnected || result.Result.RelayActive == nil || !*result.Result.RelayActive || result.Result.RelayPaused {
		t.Fatalf("integrated prepare result = %#v", result)
	}
	if lifecycle.prepareCalls != 1 || lifecycle.lastTicket != strings.Repeat("A", 43) || lifecycle.stateCalls != 0 {
		t.Fatalf("lifecycle calls = prepare:%d state:%d", lifecycle.prepareCalls, lifecycle.stateCalls)
	}
}

func TestProcessorRoutesPauseResumeAndDisconnectToIntegratedRuntime(t *testing.T) {
	t.Parallel()
	lifecycle := &controlRuntimeStub{state: installation.LocalState{RelayPaused: true}}
	processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	for _, action := range []Action{ActionPause, ActionResume, ActionRepair, ActionDisconnect} {
		result := processor.Handle(context.Background(), Request{ID: uint64(lifecycle.stateCalls + 2), Version: "1", Action: action, Scope: LifecycleScope})
		if !result.OK || result.Result == nil || !result.Result.RelayPaused {
			t.Fatalf("%s = %#v", action, result)
		}
	}
	if lifecycle.stateCalls != 4 || lifecycle.repairCalls != 1 || lifecycle.disconnectCalls != 1 {
		t.Fatalf("lifecycle calls = %d, repair calls=%d, disconnect calls=%d", lifecycle.stateCalls, lifecycle.repairCalls, lifecycle.disconnectCalls)
	}
	wrongScope := processor.Handle(context.Background(), Request{ID: 5, Version: "1", Action: ActionDisconnect, Scope: "workspace:a"})
	if wrongScope.Error != ErrorInvalidRequest || lifecycle.disconnectCalls != 1 {
		t.Fatalf("workspace-scoped disconnect = %#v, disconnect calls=%d", wrongScope, lifecycle.disconnectCalls)
	}
}

func TestProcessorPreservesFiniteLifecycleLockErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		code string
		want ErrorCode
	}{
		{name: "locked", code: "session_locked", want: ErrorSessionLocked},
		{name: "unknown", code: "session_state_unknown", want: ErrorSessionStateUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lifecycle := &controlRuntimeStub{prepareErr: finiteControlError{code: test.code}}
			processor, err := NewWithControlRuntime(&serviceStub{}, nil, lifecycle, "https://my.personastack.ai")
			if err != nil {
				t.Fatal(err)
			}
			processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
			result := processor.Handle(context.Background(), Request{
				ID: 2, Version: "1", Action: ActionPrepare, Scope: "workspace:a", EnrollmentTicket: strings.Repeat("A", 43),
			})
			if result.OK || result.Error != test.want {
				t.Fatalf("prepare = %#v, want error %q", result, test.want)
			}
		})
	}
}

func TestProcessorScopeChangeCancelsInFlightCommand(t *testing.T) {
	t.Parallel()
	service := &blockingService{started: make(chan struct{}), canceled: make(chan struct{})}
	processor, err := New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	finished := make(chan Response, 1)
	go func() {
		finished <- processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionState, Scope: "workspace:a"})
	}()
	<-service.started
	processor.Handle(context.Background(), Request{ID: 3, Version: "1", Action: ActionSync, Scope: "workspace:b"})
	if got := <-finished; got.Error != ErrorStaleRequest {
		t.Fatalf("in-flight state after scope change = %#v", got)
	}
	select {
	case <-service.canceled:
	default:
		t.Fatal("scope change did not cancel the service context")
	}
	if got := processor.Handle(context.Background(), Request{ID: 4, Version: "1", Action: ActionState, Scope: "workspace:a"}); got.Error != ErrorInvalidRequest {
		t.Fatalf("old-scope state = %#v", got)
	}
}

func TestProcessorMapsLocalStateErrorsToFiniteCodes(t *testing.T) {
	t.Parallel()
	service := &serviceStub{err: credentialstore.ErrUnavailable}
	processor, err := New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync})
	response := processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionState})
	if response.OK || response.Error != ErrorKeyringUnavailable || service.calls != 1 {
		t.Fatalf("response = %#v, service calls %d", response, service.calls)
	}
}

func TestProcessorRoutesLocalSessionThroughItsOwnScopeContract(t *testing.T) {
	t.Parallel()
	local := &localSessionStub{result: json.RawMessage(`{"ok":true,"version":"2"}`)}
	processor, err := NewWithLocalSessions(&serviceStub{}, local, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	command, err := localsession.Parse([]byte(`{"version":"1","action":"state","scope":"workspace:a"}`))
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ID: 1, Version: "1", Action: ActionLocalSession, Scope: "workspace:a", LocalSession: json.RawMessage(`{"version":"1","action":"state","scope":"workspace:a"}`)}
	result := processor.Handle(context.Background(), request)
	if !result.OK || string(result.LocalSession) != string(local.result) || local.calls != 1 || local.origin != "https://my.personastack.ai" || local.command.Action != command.Action {
		t.Fatalf("local-session result = %#v, calls = %d, origin = %q", result, local.calls, local.origin)
	}
	request.Scope = "workspace:b"
	denied := processor.Handle(context.Background(), request)
	if denied.OK || denied.Error != ErrorInvalidRequest || local.calls != 1 {
		t.Fatalf("wrong-scope local-session result = %#v, calls = %d", denied, local.calls)
	}
}

func TestProcessorMapsLocalSessionErrorsToFiniteCodes(t *testing.T) {
	t.Parallel()
	local := &localSessionStub{err: localsession.ErrMissingHarness}
	processor, err := NewWithLocalSessions(&serviceStub{}, local, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	processor.Handle(context.Background(), Request{ID: 1, Version: "1", Action: ActionSync, Scope: "workspace:a"})
	result := processor.Handle(context.Background(), Request{ID: 2, Version: "1", Action: ActionLocalSession, Scope: "workspace:a", LocalSession: json.RawMessage(`{"version":"1","action":"state","scope":"workspace:a"}`)})
	if result.OK || result.Error != ErrorMissingHarness || local.calls != 1 {
		t.Fatalf("local error result = %#v, calls = %d", result, local.calls)
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

type serviceStub struct {
	calls  int
	origin string
	state  installation.LocalState
	err    error
}

type controlRuntimeStub struct {
	state               installation.LocalState
	prepareCalls        int
	stateCalls          int
	lifecycleStateCalls int
	lastTicket          string
	prepareErr          error
	disconnectCalls     int
	repairCalls         int
	pauseStarted        chan struct{}
	pauseRelease        chan struct{}
}

func (runtime *controlRuntimeStub) LocalState(context.Context, string) (installation.LocalState, error) {
	runtime.stateCalls++
	return runtime.state, nil
}

func (runtime *controlRuntimeStub) LifecycleState(context.Context, string) (installation.LocalState, error) {
	runtime.lifecycleStateCalls++
	return runtime.state, nil
}

func (runtime *controlRuntimeStub) Prepare(_ context.Context, _ string, ticket string) (installation.LocalState, error) {
	runtime.prepareCalls++
	runtime.lastTicket = ticket
	return runtime.state, runtime.prepareErr
}

func (runtime *controlRuntimeStub) Pause(ctx context.Context) (installation.LocalState, error) {
	runtime.stateCalls++
	if runtime.pauseStarted != nil {
		close(runtime.pauseStarted)
	}
	if runtime.pauseRelease != nil {
		select {
		case <-runtime.pauseRelease:
		case <-ctx.Done():
			return installation.LocalState{}, ctx.Err()
		}
	}
	return runtime.state, nil
}

func (runtime *controlRuntimeStub) Resume(context.Context) (installation.LocalState, error) {
	runtime.stateCalls++
	return runtime.state, nil
}

func (runtime *controlRuntimeStub) Repair(context.Context) (installation.LocalState, error) {
	runtime.stateCalls++
	runtime.repairCalls++
	return runtime.state, nil
}

func (runtime *controlRuntimeStub) Disconnect(context.Context) (installation.LocalState, error) {
	runtime.stateCalls++
	runtime.disconnectCalls++
	return runtime.state, nil
}

type finiteControlError struct{ code string }

func (err finiteControlError) Error() string                   { return "finite lifecycle error" }
func (err finiteControlError) DesktopControlErrorCode() string { return err.code }

type blockingService struct {
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (s *blockingService) LocalState(ctx context.Context, _ string) (installation.LocalState, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	close(s.canceled)
	return installation.LocalState{}, ctx.Err()
}

type localSessionStub struct {
	calls   int
	origin  string
	command localsession.Command
	result  json.RawMessage
	err     error
}

func (s *localSessionStub) SynchronizeScope(string) {}

func (s *localSessionStub) Handle(_ context.Context, origin string, command localsession.Command) (json.RawMessage, error) {
	s.calls++
	s.origin = origin
	s.command = command
	return s.result, s.err
}

func (s *serviceStub) LocalState(_ context.Context, origin string) (installation.LocalState, error) {
	s.calls++
	s.origin = origin
	return s.state, s.err
}

func TestErrorCodeMapsServiceErrors(t *testing.T) {
	t.Parallel()
	if got := errorCode(credentialstore.ErrCredentialMissing); got != ErrorNotEnrolled {
		t.Fatalf("missing credential code = %q", got)
	}
	if got := errorCode(desktopcontrol.ErrRejected); got != ErrorRejected {
		t.Fatalf("rejected code = %q", got)
	}
	if got := errorCode(errors.New("private API body")); got != ErrorUnavailable {
		t.Fatalf("unknown error code = %q", got)
	}
}
