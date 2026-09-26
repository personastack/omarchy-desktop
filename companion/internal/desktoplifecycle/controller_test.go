//go:build linux

package desktoplifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/cuaruntime"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopexecutor"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopgateway"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

type runtimeFake struct {
	mu           sync.Mutex
	prepareCalls int
	stopCalls    int
	closed       bool
}

func (runtime *runtimeFake) Prepare(context.Context) (cuaruntime.State, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.prepareCalls++
	return cuaruntime.State{Ready: true, DriverVersion: "0.29.1", ToolCount: 33}, nil
}

func (*runtimeFake) Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (runtime *runtimeFake) State() cuaruntime.State {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return cuaruntime.State{Ready: true, DriverVersion: "0.29.1", ToolCount: 33}
}

func (runtime *runtimeFake) Close() {
	runtime.mu.Lock()
	runtime.closed = true
	runtime.mu.Unlock()
}

func (runtime *runtimeFake) Stop() {
	runtime.mu.Lock()
	runtime.stopCalls++
	runtime.mu.Unlock()
}

type installServiceFake struct {
	mu           sync.Mutex
	installation desktopcontrol.Installation
	missing      bool
	active       bool
	readiness    []apicontract.DesktopControlReadiness
	attached     int
	enrolled     int
	prepared     int
	claimed      []string
	localState   installation.LocalState
	record       chan string
	claimReady   chan struct{}
}

func (service *installServiceFake) StoredInstallation(context.Context, string) (desktopcontrol.Installation, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.missing {
		return desktopcontrol.Installation{}, credentialstore.ErrCredentialMissing
	}
	return service.installation, nil
}

func (service *installServiceFake) Enroll(_ context.Context, _ string, operatingSystem apicontract.DesktopControlOperatingSystem) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if operatingSystem != apicontract.DesktopControlOperatingSystemLinux {
		return errors.New("wrong operating system")
	}
	service.enrolled++
	service.missing = false
	service.installation = desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}
	return nil
}

func (service *installServiceFake) Attach(context.Context, string, string) error {
	service.mu.Lock()
	service.attached++
	service.mu.Unlock()
	return nil
}

func (service *installServiceFake) ReportReadiness(_ context.Context, _ string, readiness apicontract.DesktopControlReadiness) error {
	service.mu.Lock()
	service.readiness = append(service.readiness, readiness)
	service.mu.Unlock()
	return nil
}

func (service *installServiceFake) Status(context.Context, string) (installation.Status, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return installation.Status{Enrolled: true, CredentialValid: true, RelayActive: service.active}, nil
}

func (service *installServiceFake) PrepareSession(context.Context, string) (int64, error) {
	service.mu.Lock()
	service.prepared++
	if service.record != nil {
		service.record <- "prepare"
	}
	service.mu.Unlock()
	return 7, nil
}

func (service *installServiceFake) ClaimSession(_ context.Context, _, sessionID string, generation int64) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if generation != 7 {
		return errors.New("wrong generation")
	}
	service.claimed = append(service.claimed, sessionID)
	if service.record != nil {
		service.record <- "claim"
	}
	if service.claimReady != nil {
		close(service.claimReady)
		service.claimReady = nil
	}
	return nil
}

func (service *installServiceFake) LocalState(context.Context, string) (installation.LocalState, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	state := service.localState
	if state.InstallationID == nil && service.installation.InstallationID != "" {
		state.InstallationID = &service.installation.InstallationID
	}
	return state, nil
}

type lockProbeFake struct {
	state desktopexecutor.SessionLockState
	err   error
}

type pausePreferenceFake struct {
	paused bool
	err    error
}

func (preference *pausePreferenceFake) Load(string) (bool, error) {
	return preference.paused, preference.err
}
func (preference *pausePreferenceFake) Save(_ string, paused bool) error {
	if preference.err != nil {
		return preference.err
	}
	preference.paused = paused
	return nil
}

func (probe lockProbeFake) State(context.Context) (desktopexecutor.SessionLockState, error) {
	return probe.state, probe.err
}

type localOperationsFake struct{}

func (localOperationsFake) Call(context.Context, agentgatewayruntime.DesktopControlOperation, json.RawMessage, time.Duration) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (localOperationsFake) ActiveProcesses() int          { return 0 }
func (localOperationsFake) CloseAll(context.Context) bool { return true }

type blockingLocalOperations struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type lifecycleBlockingLocalOperations struct{ state *blockingLocalOperations }

func (local lifecycleBlockingLocalOperations) Call(context.Context, agentgatewayruntime.DesktopControlOperation, json.RawMessage, time.Duration) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (local lifecycleBlockingLocalOperations) ActiveProcesses() int { return 0 }
func (local lifecycleBlockingLocalOperations) CloseAll(ctx context.Context) bool {
	local.state.once.Do(func() { close(local.state.started) })
	select {
	case <-local.state.release:
		return true
	case <-ctx.Done():
		return false
	}
}

type retryCloseLocalOperations struct {
	mu    sync.Mutex
	calls int
}

func (*retryCloseLocalOperations) Call(context.Context, agentgatewayruntime.DesktopControlOperation, json.RawMessage, time.Duration) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (*retryCloseLocalOperations) ActiveProcesses() int { return 0 }
func (local *retryCloseLocalOperations) CloseAll(context.Context) bool {
	local.mu.Lock()
	defer local.mu.Unlock()
	local.calls++
	return local.calls > 1
}

type lockMonitorFake struct{ sink LockStateSink }

func (monitor lockMonitorFake) Run(ctx context.Context) error {
	monitor.sink.SetSessionLockState(ctx, desktopexecutor.SessionLockUnlocked)
	<-ctx.Done()
	return ctx.Err()
}

type lockMonitorIdle struct{}

func (lockMonitorIdle) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type gatewayFake struct {
	mu            sync.Mutex
	connected     bool
	readiness     string
	lastHeartbeat time.Time
	deferAck      bool
	readyWritten  chan struct{}
	readyOnce     sync.Once
	closed        chan struct{}
	once          sync.Once
	record        chan string
}

func (connection *gatewayFake) Connect(context.Context) error {
	if connection.record != nil {
		connection.record <- "connect"
	}
	connection.mu.Lock()
	connection.connected = true
	connection.lastHeartbeat = time.Now().Add(-time.Millisecond)
	connection.mu.Unlock()
	return nil
}
func (connection *gatewayFake) ConnectWithLifetime(handshake, lifetime context.Context) error {
	if handshake == nil || lifetime == nil {
		return ErrUnavailable
	}
	if err := handshake.Err(); err != nil {
		return err
	}
	if err := lifetime.Err(); err != nil {
		return err
	}
	return connection.Connect(lifetime)
}
func (connection *gatewayFake) Status() desktopgateway.Status {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return desktopgateway.Status{Connected: connection.connected, Readiness: connection.readiness, LastHeartbeatAt: connection.lastHeartbeat}
}
func (connection *gatewayFake) SetReadiness(readiness string) error {
	connection.mu.Lock()
	connection.readiness = readiness
	if !connection.deferAck {
		connection.lastHeartbeat = time.Now()
	}
	connection.mu.Unlock()
	if connection.readyWritten != nil {
		connection.readyOnce.Do(func() { close(connection.readyWritten) })
	}
	return nil
}
func (connection *gatewayFake) AcknowledgeReadiness() {
	connection.mu.Lock()
	connection.lastHeartbeat = time.Now()
	connection.mu.Unlock()
}
func (connection *gatewayFake) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-connection.closed:
		return desktopgateway.ErrConnectionEnded
	}
}
func (connection *gatewayFake) Close() {
	connection.once.Do(func() { close(connection.closed) })
	connection.mu.Lock()
	connection.connected = false
	connection.mu.Unlock()
}

func TestPrepareRejectsLockedOrUnknownSessionBeforeStartingCua(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		state desktopexecutor.SessionLockState
		err   error
		want  error
	}{
		{name: "locked", state: desktopexecutor.SessionLockLocked, want: ErrLocked},
		{name: "unknown", state: desktopexecutor.SessionLockUnknown, want: ErrSession},
		{name: "probe failure", state: desktopexecutor.SessionLockUnknown, err: errors.New("hyprctl failed"), want: ErrSession},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runtime := &runtimeFake{}
			service := &installServiceFake{}
			controller := newTestController(t, runtime, service, lockProbeFake{state: test.state, err: test.err}, nil)
			if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); !errors.Is(err, test.want) {
				t.Fatalf("Prepare() = %v, want %v", err, test.want)
			}
			runtime.mu.Lock()
			calls := runtime.prepareCalls
			runtime.mu.Unlock()
			if calls != 0 || service.enrolled != 0 || service.attached != 0 {
				t.Fatalf("locked prepare started work: Cua=%d enroll=%d attach=%d", calls, service.enrolled, service.attached)
			}
		})
	}
}

func TestPrepareEnrollsAndConnectsBeforeFirstWorkspaceConfigExists(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true, localState: installation.LocalState{RelayPaused: true}}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	options.NewGateway = func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	state, err := controller.Prepare(context.Background(), testOrigin, "ticket")
	if err != nil {
		t.Fatal(err)
	}
	if service.enrolled != 1 || service.attached != 0 || service.prepared != 0 || len(service.claimed) != 0 || state.InstallationID == nil || !state.CuaReady || !state.NativeExecutorReady || !state.GatewayConnected || state.RelayPaused {
		t.Fatalf("prepare state=%#v enroll=%d attach=%d session=%d", state, service.enrolled, service.attached, service.prepared)
	}
	if len(service.readiness) != 1 || service.readiness[0] != apicontract.DesktopControlReadinessReady {
		t.Fatalf("readiness reports = %v", service.readiness)
	}
	if !controller.Close(context.Background()) {
		t.Fatal("Close() failed")
	}
	runtime.mu.Lock()
	closed := runtime.closed
	runtime.mu.Unlock()
	if !closed {
		t.Fatal("Close() did not stop the Cua runtime")
	}
}

func TestPausePersistsAcrossRestartAndResumeRechecksUnlockedActiveInstall(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true, active: true}
	var connection *gatewayFake
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			connection = &gatewayFake{closed: make(chan struct{})}
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatal(err)
	}
	state, err := controller.Pause(context.Background())
	if err != nil || !state.RelayPaused || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("Pause() state=%#v err=%v readiness=%q", state, err, connection.Status().Readiness)
	}
	if paused, err := preference.Load(testOrigin); err != nil || !paused {
		t.Fatalf("saved pause = %t, %v", paused, err)
	}
	if !controller.Close(context.Background()) {
		t.Fatal("Close() failed")
	}

	restartedRuntime := &runtimeFake{}
	restarted := options
	restarted.Runtime = restartedRuntime
	restarted.Installations = &installServiceFake{active: true}
	restarted.NewGateway = nil
	restartedController, err := New(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedController.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	restartedRuntime.mu.Lock()
	prepareCalls := restartedRuntime.prepareCalls
	restartedRuntime.mu.Unlock()
	if prepareCalls != 0 {
		t.Fatalf("restart recovered a user-paused runtime %d times", prepareCalls)
	}
	if !restartedController.Close(context.Background()) {
		t.Fatal("restarted controller Close() failed")
	}

	resumedRuntime := &runtimeFake{}
	resumeOptions := options
	resumeOptions.Runtime = resumedRuntime
	resumeOptions.Installations = &installServiceFake{active: false}
	resumeController, err := New(resumeOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeController.Resume(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Resume() without active install = %v, want unavailable", err)
	}
	if !resumeController.Close(context.Background()) {
		t.Fatal("inactive resume controller Close() failed")
	}
	resumeOptions.Installations = service
	resumeController, err = New(resumeOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeController.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	if !resumeController.Close(context.Background()) {
		t.Fatal("resumed controller Close() failed")
	}
	if paused, err := preference.Load(testOrigin); err != nil || paused {
		t.Fatalf("cleared pause = %t, %v", paused, err)
	}
}

func TestPrepareConnectsThroughGatewayOwnedSessionLifecycle(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, record: make(chan string, 4), claimReady: make(chan struct{}), installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{}), record: service.record}
	controller := newTestController(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	})
	state, err := controller.Prepare(context.Background(), testOrigin, "ticket")
	if err != nil {
		t.Fatal(err)
	}
	state, err = controller.State(context.Background())
	if err != nil || !state.GatewayConnected {
		t.Fatalf("Gateway state = %#v, %v", state, err)
	}
	if !state.NativeExecutorReady || state.RelayPaused {
		t.Fatalf("State() = %#v", state)
	}
	service.mu.Lock()
	prepared, claims := service.prepared, append([]string(nil), service.claimed...)
	service.mu.Unlock()
	if prepared != 0 || len(claims) != 0 {
		t.Fatalf("session preparation/claim = %d, %v", prepared, claims)
	}
	var order []string
	for len(service.record) > 0 {
		order = append(order, <-service.record)
	}
	if len(order) != 1 || order[0] != "connect" {
		t.Fatalf("Gateway session call order = %v", order)
	}
	if !controller.Close(context.Background()) {
		t.Fatal("Close() failed")
	}
}

func TestPrepareWaitsForGatewayHeartbeatAcknowledgement(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{}), deferAck: true, readyWritten: make(chan struct{})}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	options.NewGateway = func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	result := make(chan error, 1)
	go func() {
		_, err := controller.Prepare(context.Background(), testOrigin, "ticket")
		result <- err
	}()
	select {
	case <-connection.readyWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway readiness was not written")
	}
	select {
	case err := <-result:
		t.Fatalf("Prepare returned before heartbeat acknowledgement: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	connection.AcknowledgeReadiness()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Prepare after heartbeat acknowledgement: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare did not return after heartbeat acknowledgement")
	}
}

func TestPrepareRejectsLockTransitionBeforeGatewayAcknowledgement(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{}), deferAck: true, readyWritten: make(chan struct{})}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	options.NewGateway = func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	result := make(chan error, 1)
	go func() {
		_, err := controller.Prepare(context.Background(), testOrigin, "ticket")
		result <- err
	}()
	select {
	case <-connection.readyWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway readiness was not written")
	}
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockLocked) {
		t.Fatal("locked transition was not applied")
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("Prepare() error = %v, want locked", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare did not reject the locked session")
	}
}

func TestPrepareReusesGatewayForAnotherWorkspace(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{}), record: make(chan string, 4)}
	controller := newTestController(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	})
	for range 2 {
		state, err := controller.Prepare(context.Background(), testOrigin, "ticket")
		if err != nil || !state.GatewayConnected {
			t.Fatalf("Prepare() state=%#v error=%v", state, err)
		}
	}
	if service.attached != 2 {
		t.Fatalf("Attach calls = %d, want 2", service.attached)
	}
	var connects int
	for len(connection.record) > 0 {
		if <-connection.record == "connect" {
			connects++
		}
	}
	if connects != 1 {
		t.Fatalf("Gateway connects = %d, want 1", connects)
	}
}

func TestRecoverRestoresAnActiveInstallation(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{})}
	controller := newTestController(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	})
	if err := controller.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		state, err := controller.State(context.Background())
		if err == nil && state.GatewayConnected && state.NativeExecutorReady && !state.RelayPaused {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("recovered state=%#v error=%v", state, err)
		case <-time.After(time.Millisecond):
		}
	}
	if service.enrolled != 0 || service.attached != 0 {
		t.Fatalf("startup recovery changed enrollment: enroll=%d attach=%d", service.enrolled, service.attached)
	}
}

func TestGatewayDisconnectKeepsLeaseFencedBeforeReconnect(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{}
	localState := &blockingLocalOperations{started: make(chan struct{}), release: make(chan struct{})}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: lifecycleBlockingLocalOperations{state: localState}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, options.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !executor.SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("could not unlock test executor")
	}
	connection := &gatewayFake{closed: make(chan struct{})}
	if err := connection.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.connection = connection
	controller.mu.Unlock()
	go controller.watchGateway(connection)
	connection.Close()
	select {
	case <-localState.started:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not fence local resources")
	}
	controller.mu.Lock()
	stillPublished := controller.connection == connection
	controller.mu.Unlock()
	if !stillPublished {
		t.Fatal("Gateway cleared before executor cleanup completed")
	}
	close(localState.release)
	deadline := time.After(2 * time.Second)
	for {
		controller.mu.Lock()
		cleared := controller.connection == nil
		controller.mu.Unlock()
		if cleared {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Gateway was not cleared after executor cleanup")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCloseRetriesUnconfirmedExecutorCleanup(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{}
	local := &retryCloseLocalOperations{}
	controller := newTestController(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, local)
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.mu.Unlock()
	if controller.Close(context.Background()) {
		t.Fatal("first Close() claimed incomplete cleanup")
	}
	if !controller.Close(context.Background()) {
		t.Fatal("second Close() did not complete cleanup")
	}
	local.mu.Lock()
	calls := local.calls
	local.mu.Unlock()
	if calls != 2 {
		t.Fatalf("cleanup calls = %d, want 2", calls)
	}
}

func TestResumeIdleRelayRestartsCuaAndLockMonitor(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{}
	var monitorMu sync.Mutex
	monitorStarts := 0
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) {
		monitorMu.Lock()
		monitorStarts++
		monitorMu.Unlock()
		return lockMonitorIdle{}, nil
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if err := controller.startExecutorAndMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.paused = false
	controller.mu.Unlock()
	controller.stopIdleRelay()
	service.mu.Lock()
	service.active = true
	service.mu.Unlock()
	if err := controller.resumeIdleRelay(context.Background()); err != nil {
		t.Fatalf("resumeIdleRelay() error = %v", err)
	}
	controller.mu.Lock()
	paused := controller.paused
	executorReady := controller.executor.NativeReady()
	controller.mu.Unlock()
	if paused || !executorReady {
		t.Fatalf("resumed relay paused=%t executorReady=%t", paused, executorReady)
	}
	runtime.mu.Lock()
	prepareCalls, stopCalls := runtime.prepareCalls, runtime.stopCalls
	runtime.mu.Unlock()
	monitorMu.Lock()
	starts := monitorStarts
	monitorMu.Unlock()
	if prepareCalls != 1 || stopCalls != 1 || starts != 2 {
		t.Fatalf("runtime prepare/stop=%d/%d and monitor starts=%d", prepareCalls, stopCalls, starts)
	}
}

func TestStartupIdleInstallationReconnectsWhenMappingBecomesActive(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}, ReconnectEvery: 10 * time.Millisecond}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	options.NewGateway = func(_ desktopcontrol.Installation, _ string, _ desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		return connection, nil
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	controller.StartRecovery()
	deadline := time.After(2 * time.Second)
	for {
		controller.mu.Lock()
		checked := controller.recoveryChecked
		controller.mu.Unlock()
		if checked {
			break
		}
		select {
		case <-deadline:
			t.Fatal("startup did not confirm the initial idle installation")
		case <-time.After(time.Millisecond):
		}
	}
	service.mu.Lock()
	service.active = true
	service.mu.Unlock()
	for {
		state, err := controller.State(context.Background())
		if err == nil && state.GatewayConnected && state.NativeExecutorReady && !state.RelayPaused {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("startup idle→active state=%#v error=%v", state, err)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestLockCleanupFailureStillReportsObservedLockState(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{}
	local := &retryCloseLocalOperations{}
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: local, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil }
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, local)
	if err != nil {
		t.Fatal(err)
	}
	if !executor.SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("could not unlock test executor")
	}
	connection := &gatewayFake{closed: make(chan struct{})}
	if err := connection.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.connection = connection
	controller.currentLock = desktopexecutor.SessionLockUnlocked
	controller.mu.Unlock()
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockLocked) {
		t.Fatal("monitor stopped after cleanup failure")
	}
	if readiness := connection.Status().Readiness; readiness != string(apicontract.DesktopControlReadinessLocked) {
		t.Fatalf("Gateway readiness = %q, want locked", readiness)
	}
	if executor.NativeReady() {
		t.Fatal("executor remained ready after failed cleanup")
	}
}

const testOrigin = "https://my.personastack.ai"

func newTestController(t *testing.T, runtime cuaRuntime, service *installServiceFake, probe lockProbe, newGateway gatewayFactory) *Controller {
	t.Helper()
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: probe, Local: localOperationsFake{}, PausePreference: &pausePreferenceFake{}}
	options.NewLockMonitor = func(sink LockStateSink, _ time.Duration) (LockMonitor, error) {
		return lockMonitorFake{sink: sink}, nil
	}
	if newGateway != nil {
		options.NewGateway = newGateway
	}
	controller, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	return controller
}
