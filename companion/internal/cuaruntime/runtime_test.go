package cuaruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/cuamcp"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

func TestPrepareStartsPinnedDriverAndRequiresLinuxHealth(t *testing.T) {
	t.Parallel()
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click", "get_desktop_state"}}, report: readyReport()}
	installer := &installerStub{path: "/owned/cua-driver"}
	factoryCalls := 0
	runtime, err := New(installer, func(_ context.Context, path string) (Client, error) {
		factoryCalls++
		if path != installer.path {
			t.Fatalf("factory executable = %q", path)
		}
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	state, err := runtime.Prepare(context.Background())
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if state != (State{Ready: true, DriverVersion: "0.30.1", ToolCount: 2}) || runtime.State() != state {
		t.Fatalf("state = %#v, current = %#v", state, runtime.State())
	}
	if installer.calls != 1 || factoryCalls != 1 || client.starts != 1 || client.permissionReads != 1 || client.healthReads != 1 || client.stops != 0 {
		t.Fatalf("calls install=%d factory=%d start=%d permissions=%d health=%d stop=%d", installer.calls, factoryCalls, client.starts, client.permissionReads, client.healthReads, client.stops)
	}

	arguments := json.RawMessage(`{"pid":42}`)
	result, err := runtime.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", arguments)
	if err != nil || string(result) != `{"ok":true}` || client.lastTool != "get_desktop_state" || string(client.lastArguments) != string(arguments) {
		t.Fatalf("Call() = %s, %v; tool=%q arguments=%s", result, err, client.lastTool, client.lastArguments)
	}
	second, err := runtime.Prepare(context.Background())
	if err != nil || second != state || installer.calls != 1 || client.starts != 1 {
		t.Fatalf("second Prepare() = %#v, %v; install=%d start=%d", second, err, installer.calls, client.starts)
	}
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.Prepare(canceledContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare() with canceled context and cached readiness error = %v", err)
	}
	runtime.Close()
	if runtime.State() != (State{}) || client.stops != 1 {
		t.Fatalf("Close() state=%#v stops=%d", runtime.State(), client.stops)
	}
}

func TestPrepareKeepsChildAliveAfterSetupContextIsCanceled(t *testing.T) {
	t.Parallel()
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"get_desktop_state"}}, report: readyReport()}
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return client, nil })
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := runtime.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-client.lifetimeDone:
		t.Fatal("setup cancellation canceled the child lifetime")
	default:
	}
	if _, err := runtime.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call() after setup context cancellation = %v", err)
	}
	if !client.Alive() || !runtime.State().Ready {
		t.Fatalf("setup context cancellation stopped accepted child: alive=%t state=%#v", client.Alive(), runtime.State())
	}
	runtime.Close()
	select {
	case <-client.lifetimeDone:
	default:
		t.Fatal("Close() did not cancel the child lifetime")
	}
}

func TestRepairStopsOwnedChildAndStartsAValidatedReplacement(t *testing.T) {
	t.Parallel()
	installer := &installerStub{path: "/owned/cua-driver"}
	first := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport()}
	second := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport()}
	factoryCalls := 0
	runtime := mustRuntime(t, installer, func(context.Context, string) (Client, error) {
		factoryCalls++
		if factoryCalls == 1 {
			return first, nil
		}
		return second, nil
	})
	if _, err := runtime.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := runtime.Repair(context.Background())
	if err != nil || !state.Ready || runtime.State() != state {
		t.Fatalf("Repair() = %#v, %v", state, err)
	}
	if installer.repairCalls != 1 || installer.calls != 1 || first.stops != 1 || second.starts != 1 || !second.Alive() {
		t.Fatalf("installer install/repair=%d/%d, old stops=%d, replacement starts=%d alive=%t", installer.calls, installer.repairCalls, first.stops, second.starts, second.Alive())
	}
	runtime.Close()
}

func TestPrepareQueuedCallReturnsWhenItsContextIsCanceled(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	installer := &installerStub{path: "/owned/cua-driver", install: func(ctx context.Context) (string, error) {
		close(entered)
		select {
		case <-release:
			return "/owned/cua-driver", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	runtime := mustRuntime(t, installer, func(context.Context, string) (Client, error) {
		return &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport()}, nil
	})
	firstResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(context.Background())
		firstResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first installer did not start")
	}
	queuedContext, cancel := context.WithCancel(context.Background())
	queuedResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(queuedContext)
		queuedResult <- err
	}()
	cancel()
	select {
	case err := <-queuedResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Prepare() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued Prepare() ignored context cancellation")
	}
	close(release)
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("first Prepare() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first Prepare() did not finish")
	}
	runtime.Close()
}

func TestCloseCancelsBlockedInstallWithoutStartingChild(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	installer := &installerStub{path: "/owned/cua-driver", install: func(ctx context.Context) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	factoryCalls := 0
	runtime := mustRuntime(t, installer, func(context.Context, string) (Client, error) {
		factoryCalls++
		return &clientStub{}, nil
	})
	prepareResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(context.Background())
		prepareResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("installer did not start")
	}
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() blocked on installation")
	}
	select {
	case err := <-prepareResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prepare() did not stop after Close()")
	}
	if factoryCalls != 0 || installer.calls != 1 || runtime.State() != (State{}) {
		t.Fatalf("factory calls=%d installer calls=%d state=%#v", factoryCalls, installer.calls, runtime.State())
	}
	if _, err := runtime.Prepare(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Prepare() after Close() error = %v", err)
	}
}

func TestCloseCancelsBlockedClientStartAndStopsChild(t *testing.T) {
	t.Parallel()
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport(), startEntered: make(chan struct{}), blockStart: true}
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return client, nil })
	prepareResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(context.Background())
		prepareResult <- err
	}()
	select {
	case <-client.startEntered:
	case <-time.After(time.Second):
		t.Fatal("Cua client startup did not begin")
	}
	closeResult := make(chan struct{})
	go func() {
		runtime.Close()
		close(closeResult)
	}()
	select {
	case <-closeResult:
	case <-time.After(time.Second):
		t.Fatal("Close() blocked on Cua startup")
	}
	select {
	case err := <-prepareResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prepare() did not stop after Close()")
	}
	if runtime.State() != (State{}) || client.stops == 0 || client.Alive() {
		t.Fatalf("state=%#v stops=%d alive=%t", runtime.State(), client.stops, client.Alive())
	}
}

func TestCloseCancelsBlockedHealthProbeAndStopsChild(t *testing.T) {
	t.Parallel()
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport(), healthEntered: make(chan struct{}), blockHealth: true}
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return client, nil })
	prepareResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(context.Background())
		prepareResult <- err
	}()
	select {
	case <-client.healthEntered:
	case <-time.After(time.Second):
		t.Fatal("Cua health probe did not begin")
	}
	closeResult := make(chan struct{})
	go func() {
		runtime.Close()
		close(closeResult)
	}()
	select {
	case <-closeResult:
	case <-time.After(time.Second):
		t.Fatal("Close() blocked on Cua health probe")
	}
	select {
	case err := <-prepareResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prepare() did not stop after Close()")
	}
	if runtime.State() != (State{}) || client.stops == 0 || client.Alive() {
		t.Fatalf("state=%#v stops=%d alive=%t", runtime.State(), client.stops, client.Alive())
	}
}

func TestPrepareRejectsChildExitDuringHealthProbe(t *testing.T) {
	t.Parallel()
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport(), exitAfterHealth: true}
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return client, nil })
	if _, err := runtime.Prepare(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Prepare() error = %v, want not ready", err)
	}
	if runtime.State() != (State{}) || client.stops != 1 {
		t.Fatalf("state=%#v stops=%d", runtime.State(), client.stops)
	}
}

func TestCloseDuringFactoryPreventsStartingLateChild(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(ctx context.Context, _ string) (Client, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	prepareResult := make(chan error, 1)
	go func() {
		_, err := runtime.Prepare(context.Background())
		prepareResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Cua client factory did not begin")
	}
	runtime.Close()
	select {
	case err := <-prepareResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prepare() did not stop after Close()")
	}
	if runtime.State() != (State{}) {
		t.Fatalf("state=%#v", runtime.State())
	}
}

func TestChildExitClearsReadinessAndAllowsExplicitPrepare(t *testing.T) {
	t.Parallel()
	first := &clientStub{catalog: cuamcp.Catalog{Available: []string{"get_desktop_state"}}, report: readyReport()}
	second := &clientStub{catalog: cuamcp.Catalog{Available: []string{"get_desktop_state"}}, report: readyReport()}
	clients := []Client{first, second}
	factoryCalls := 0
	installer := &installerStub{path: "/owned/cua-driver"}
	runtime := mustRuntime(t, installer, func(context.Context, string) (Client, error) {
		client := clients[factoryCalls]
		factoryCalls++
		return client, nil
	})
	if _, err := runtime.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.alive.Store(false)
	if got := runtime.State(); got != (State{}) || first.stops != 1 {
		t.Fatalf("State() after child exit = %#v, stops=%d", got, first.stops)
	}
	if _, err := runtime.Prepare(context.Background()); err != nil || !runtime.State().Ready || second.starts != 1 {
		t.Fatalf("Prepare() after child exit error=%v state=%#v starts=%d", err, runtime.State(), second.starts)
	}
	runtime.Close()
}

func TestPrepareStopsDriverWhenHealthDoesNotMatchPinnedLinuxRuntime(t *testing.T) {
	t.Parallel()
	badReport := readyReport()
	badReport.DriverVersion = "0.30.0"
	client := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: badReport}
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return client, nil })
	state, err := runtime.Prepare(context.Background())
	if !errors.Is(err, ErrUpgradeRequired) || state != (State{}) || runtime.State() != (State{UpgradeRequired: true}) || client.stops != 1 {
		t.Fatalf("Prepare() = %#v, %v; current=%#v stops=%d", state, err, runtime.State(), client.stops)
	}
	if _, err := runtime.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Call() before readiness error = %v", err)
	}
}

func TestProtocolMismatchPersistsTypedUpgradeStateUntilRepairSucceeds(t *testing.T) {
	t.Parallel()
	first := &clientStub{startErr: cuamcp.ErrProtocolMismatch}
	second := &clientStub{startErr: errors.New("still incompatible")}
	third := &clientStub{startErr: errors.New("retry still fails")}
	fourth := &clientStub{catalog: cuamcp.Catalog{Available: []string{"click"}}, report: readyReport()}
	factoryCalls := 0
	runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) {
		factoryCalls++
		switch factoryCalls {
		case 1:
			return first, nil
		case 2:
			return second, nil
		case 3:
			return third, nil
		default:
			return fourth, nil
		}
	})
	if _, err := runtime.Prepare(context.Background()); !errors.Is(err, cuamcp.ErrProtocolMismatch) {
		t.Fatalf("Prepare() error = %v, want protocol mismatch", err)
	}
	if got := runtime.State(); got != (State{UpgradeRequired: true}) {
		t.Fatalf("state after protocol mismatch = %#v", got)
	}
	runtime.Stop()
	if got := runtime.State(); got != (State{UpgradeRequired: true}) {
		t.Fatalf("state after stop = %#v", got)
	}
	if _, err := runtime.Repair(context.Background()); err == nil || runtime.State() != (State{UpgradeRequired: true}) {
		t.Fatalf("failed repair cleared update state: err=%v state=%#v", err, runtime.State())
	}
	if _, err := runtime.Prepare(context.Background()); err == nil || runtime.State() != (State{UpgradeRequired: true}) {
		t.Fatalf("failed prepare cleared update state: err=%v state=%#v", err, runtime.State())
	}
	state, err := runtime.Repair(context.Background())
	if err != nil || !state.Ready || state.UpgradeRequired || runtime.State() != state {
		t.Fatalf("Repair() = %#v, %v; current=%#v", state, err, runtime.State())
	}
	runtime.Close()
}

func TestPrepareStopsDriverWhenStartOrHealthProbeFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		client          *clientStub
		want            string
		wantPermissions int
		wantHealth      int
	}{
		{name: "start", client: &clientStub{startErr: errors.New("start failed")}, want: "start Cua MCP process"},
		{name: "permissions", client: &clientStub{permissionErr: errors.New("permissions failed")}, want: "check Cua Linux session permissions", wantPermissions: 1},
		{name: "health", client: &clientStub{healthErr: errors.New("health failed")}, want: "read Cua Linux health report", wantPermissions: 1, wantHealth: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := mustRuntime(t, &installerStub{path: "/owned/cua-driver"}, func(context.Context, string) (Client, error) { return tc.client, nil })
			_, err := runtime.Prepare(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) || tc.client.stops != 1 || tc.client.permissionReads != tc.wantPermissions || tc.client.healthReads != tc.wantHealth || runtime.State() != (State{}) {
				t.Fatalf("Prepare() error = %v; stops=%d permissions=%d health=%d state=%#v", err, tc.client.stops, tc.client.permissionReads, tc.client.healthReads, runtime.State())
			}
		})
	}
}

func TestPrepareRejectsInvalidExecutableAndCanceledContextBeforeStarting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		cancel   bool
		wantInst int
	}{
		{name: "relative path", path: "cua-driver", wantInst: 1},
		{name: "unclean path", path: "/owned/../cua-driver", wantInst: 1},
		{name: "canceled before install", path: "/owned/cua-driver", cancel: true, wantInst: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			installer := &installerStub{path: tc.path}
			factoryCalls := 0
			runtime := mustRuntime(t, installer, func(context.Context, string) (Client, error) {
				factoryCalls++
				return &clientStub{}, nil
			})
			ctx := context.Background()
			if tc.cancel {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if _, err := runtime.Prepare(ctx); err == nil {
				t.Fatal("Prepare() succeeded")
			}
			if installer.calls != tc.wantInst || factoryCalls != 0 {
				t.Fatalf("installer calls=%d factory calls=%d", installer.calls, factoryCalls)
			}
		})
	}
}

func TestNewRequiresInstallerAndFactory(t *testing.T) {
	t.Parallel()
	installer := &installerStub{}
	if _, err := New(nil, func(context.Context, string) (Client, error) { return &clientStub{}, nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(nil, factory) error = %v", err)
	}
	if _, err := New(installer, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(installer, nil) error = %v", err)
	}
}

func mustRuntime(t *testing.T, installer Installer, factory Factory) *Runtime {
	t.Helper()
	runtime, err := New(installer, factory)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func readyReport() cuamcp.HealthReportSnapshot {
	checks := []string{"binary_version", "platform_supported", "session_active", "ax_capability", "screen_capture_capability"}
	report := cuamcp.HealthReportSnapshot{SchemaVersion: "1", Platform: "linux", DriverVersion: "0.30.1", Overall: "ok"}
	for _, name := range checks {
		report.Checks = append(report.Checks, cuamcp.HealthCheck{Name: name, Status: "pass", Message: "available"})
	}
	return report
}

type installerStub struct {
	path        string
	calls       int
	repairCalls int
	install     func(context.Context) (string, error)
	repair      func(context.Context) (string, error)
}

func (i *installerStub) Repair(ctx context.Context) (string, error) {
	i.repairCalls++
	if i.repair != nil {
		return i.repair(ctx)
	}
	return i.path, nil
}

func (i *installerStub) Install(ctx context.Context) (string, error) {
	i.calls++
	if i.install != nil {
		return i.install(ctx)
	}
	return i.path, nil
}

type clientStub struct {
	catalog         cuamcp.Catalog
	report          cuamcp.HealthReportSnapshot
	startErr        error
	permissionErr   error
	healthErr       error
	starts          int
	permissionReads int
	healthReads     int
	stops           int
	lastTool        string
	lastArguments   json.RawMessage
	callErr         error
	alive           atomic.Bool
	startEntered    chan struct{}
	healthEntered   chan struct{}
	blockStart      bool
	blockHealth     bool
	exitAfterHealth bool
	lifetimeDone    <-chan struct{}
	stopOnce        sync.Once
}

func (c *clientStub) StartWithLifetime(ctx context.Context, lifetime context.Context) (cuamcp.Catalog, error) {
	c.starts++
	c.lifetimeDone = lifetime.Done()
	c.alive.Store(true)
	if c.startEntered != nil {
		close(c.startEntered)
	}
	if c.blockStart {
		<-ctx.Done()
		return cuamcp.Catalog{}, ctx.Err()
	}
	if c.startErr != nil {
		c.alive.Store(false)
		return cuamcp.Catalog{}, c.startErr
	}
	return c.catalog, nil
}

func (c *clientStub) HealthReport(ctx context.Context) (cuamcp.HealthReportSnapshot, error) {
	c.healthReads++
	if c.healthEntered != nil {
		close(c.healthEntered)
	}
	if c.blockHealth {
		<-ctx.Done()
		return cuamcp.HealthReportSnapshot{}, ctx.Err()
	}
	if c.exitAfterHealth {
		c.alive.Store(false)
	}
	if c.healthErr != nil {
		return cuamcp.HealthReportSnapshot{}, c.healthErr
	}
	return c.report, nil
}

func (c *clientStub) CheckPermissions(context.Context) (json.RawMessage, error) {
	c.permissionReads++
	if c.permissionErr != nil {
		return nil, c.permissionErr
	}
	return json.RawMessage(`{"content":[]}`), nil
}

func (c *clientStub) Call(_ context.Context, _ agentgatewayruntime.DesktopControlOperation, name string, arguments json.RawMessage) (json.RawMessage, error) {
	c.lastTool = name
	c.lastArguments = append(json.RawMessage(nil), arguments...)
	if c.callErr != nil {
		c.alive.Store(false)
		return nil, c.callErr
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (c *clientStub) Stop() {
	c.stopOnce.Do(func() {
		c.stops++
		c.alive.Store(false)
	})
}

func (c *clientStub) Alive() bool {
	return c.alive.Load()
}
