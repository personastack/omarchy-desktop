//go:build linux

package desktoplifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	mu               sync.Mutex
	state            cuaruntime.State
	prepareCalls     int
	callCalls        int
	repairCalls      int
	stopCalls        int
	closed           bool
	repairErr        error
	events           []string
	prepareBlockCall int
	prepareStarted   chan struct{}
	prepareRelease   chan struct{}
}

func (runtime *runtimeFake) Repair(context.Context) (cuaruntime.State, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.repairCalls++
	runtime.events = append(runtime.events, "repair")
	if runtime.repairErr != nil {
		return runtime.state, runtime.repairErr
	}
	runtime.state = cuaruntime.State{Ready: true, DriverVersion: "0.30.1", ToolCount: 33}
	return runtime.state, nil
}

func (runtime *runtimeFake) Prepare(context.Context) (cuaruntime.State, error) {
	runtime.mu.Lock()
	runtime.prepareCalls++
	runtime.events = append(runtime.events, "prepare")
	runtime.state = cuaruntime.State{Ready: true, DriverVersion: "0.30.1", ToolCount: 33}
	block := runtime.prepareBlockCall == runtime.prepareCalls
	started, release := runtime.prepareStarted, runtime.prepareRelease
	runtime.mu.Unlock()
	if block {
		close(started)
		<-release
	}
	return cuaruntime.State{Ready: true, DriverVersion: "0.30.1", ToolCount: 33}, nil
}

func (runtime *runtimeFake) Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error) {
	runtime.mu.Lock()
	runtime.callCalls++
	runtime.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

func (runtime *runtimeFake) callCount() int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.callCalls
}

func (runtime *runtimeFake) State() cuaruntime.State {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.state.UpgradeRequired {
		return runtime.state
	}
	return cuaruntime.State{Ready: true, DriverVersion: "0.30.1", ToolCount: 33}
}

func (runtime *runtimeFake) Close() {
	runtime.mu.Lock()
	runtime.closed = true
	runtime.mu.Unlock()
}

func (runtime *runtimeFake) Stop() {
	runtime.mu.Lock()
	runtime.stopCalls++
	runtime.events = append(runtime.events, "stop")
	runtime.state = cuaruntime.State{UpgradeRequired: runtime.state.UpgradeRequired}
	runtime.mu.Unlock()
}

type installServiceFake struct {
	mu                sync.Mutex
	installation      desktopcontrol.Installation
	missing           bool
	active            bool
	statusErr         error
	storedErr         error
	revokeErr         error
	revoked           int
	statusDeadline    time.Time
	statusStarted     chan struct{}
	statusRelease     chan struct{}
	localStateErr     error
	localStateStarted chan struct{}
	localStateRelease chan struct{}
	readiness         []apicontract.DesktopControlReadiness
	attached          int
	enrolled          int
	prepared          int
	claimed           []string
	localState        installation.LocalState
	record            chan string
	claimReady        chan struct{}
}

func (service *installServiceFake) StoredInstallation(context.Context, string) (desktopcontrol.Installation, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.storedErr != nil {
		return desktopcontrol.Installation{}, service.storedErr
	}
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
	installationID := "install_01"
	if service.enrolled > 1 {
		installationID = "install_02"
	}
	service.installation = desktopcontrol.Installation{
		InstallationID: installationID, MachineCredential: strings.Repeat("A", 43), EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://cluster-agent.personastack.ai/v1/desktop-control/ws",
	}
	return nil
}

func (service *installServiceFake) Attach(context.Context, string, string) error {
	service.mu.Lock()
	service.attached++
	service.mu.Unlock()
	return nil
}

func (service *installServiceFake) Revoke(context.Context, string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.revoked++
	if service.revokeErr != nil {
		return service.revokeErr
	}
	service.missing = true
	service.active = false
	return nil
}

func (service *installServiceFake) ReportReadiness(_ context.Context, _ string, readiness apicontract.DesktopControlReadiness) error {
	service.mu.Lock()
	service.readiness = append(service.readiness, readiness)
	service.mu.Unlock()
	return nil
}

func (service *installServiceFake) Status(ctx context.Context, _ string) (installation.Status, error) {
	service.mu.Lock()
	started := service.statusStarted
	release := service.statusRelease
	service.statusStarted = nil
	service.statusRelease = nil
	if deadline, ok := ctx.Deadline(); ok {
		service.statusDeadline = deadline
	}
	if service.statusErr != nil {
		service.mu.Unlock()
		return installation.Status{}, service.statusErr
	}
	status := installation.Status{Enrolled: true, CredentialValid: true, RelayActive: service.active}
	service.mu.Unlock()
	if started != nil {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return installation.Status{}, ctx.Err()
		}
	}
	return status, nil
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

func (service *installServiceFake) LocalState(ctx context.Context, _ string) (installation.LocalState, error) {
	service.mu.Lock()
	err := service.localStateErr
	started := service.localStateStarted
	release := service.localStateRelease
	state := service.localState
	missing := service.missing
	if !missing && state.InstallationID == nil && service.installation.InstallationID != "" {
		state.InstallationID = &service.installation.InstallationID
	}
	if missing {
		state.InstallationID = nil
	}
	service.mu.Unlock()
	if started != nil {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return installation.LocalState{}, ctx.Err()
		}
	}
	if err != nil {
		return installation.LocalState{}, err
	}
	return state, nil
}

type lockProbeFake struct {
	state desktopexecutor.SessionLockState
	err   error
}

type mutableLockProbe struct {
	mu    sync.Mutex
	state desktopexecutor.SessionLockState
}

func (probe *mutableLockProbe) State(context.Context) (desktopexecutor.SessionLockState, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.state, nil
}

func (probe *mutableLockProbe) Set(state desktopexecutor.SessionLockState) {
	probe.mu.Lock()
	probe.state = state
	probe.mu.Unlock()
}

type pausePreferenceFake struct {
	paused       bool
	err          error
	saveFalseErr error
}

func (preference *pausePreferenceFake) Load(string) (bool, error) {
	return preference.paused, preference.err
}
func (preference *pausePreferenceFake) Save(_ string, paused bool) error {
	if preference.err != nil {
		return preference.err
	}
	if !paused && preference.saveFalseErr != nil {
		return preference.saveFalseErr
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

type recordingLocalOperations struct {
	mu       sync.Mutex
	closeAll int
}

func (*recordingLocalOperations) Call(context.Context, agentgatewayruntime.DesktopControlOperation, json.RawMessage, time.Duration) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (*recordingLocalOperations) ActiveProcesses() int { return 0 }
func (local *recordingLocalOperations) CloseAll(context.Context) bool {
	local.mu.Lock()
	local.closeAll++
	local.mu.Unlock()
	return true
}
func (local *recordingLocalOperations) closeCount() int {
	local.mu.Lock()
	defer local.mu.Unlock()
	return local.closeAll
}

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
	mu                   sync.Mutex
	connected            bool
	readiness            string
	readinessSequence    uint64
	acknowledgedSequence uint64
	lastHeartbeat        time.Time
	deferAck             bool
	readyWritten         chan struct{}
	readyOnce            sync.Once
	onReadyOnce          sync.Once
	onReadiness          func(string)
	closed               chan struct{}
	once                 sync.Once
	record               chan string
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
	return desktopgateway.Status{
		Connected: connection.connected, Readiness: connection.readiness,
		ReadinessSequence:             connection.readinessSequence,
		AcknowledgedHeartbeatSequence: connection.acknowledgedSequence,
		LastHeartbeatAt:               connection.lastHeartbeat,
	}
}
func (connection *gatewayFake) SetReadiness(readiness string) error {
	connection.mu.Lock()
	connection.readiness = readiness
	connection.readinessSequence++
	if !connection.deferAck {
		connection.acknowledgedSequence = connection.readinessSequence
		connection.lastHeartbeat = time.Now()
	}
	connection.mu.Unlock()
	if connection.readyWritten != nil {
		connection.readyOnce.Do(func() { close(connection.readyWritten) })
	}
	if readiness == string(apicontract.DesktopControlReadinessReady) && connection.onReadiness != nil {
		connection.onReadyOnce.Do(func() { connection.onReadiness(readiness) })
	}
	return nil
}
func (connection *gatewayFake) AcknowledgeReadiness() {
	connection.mu.Lock()
	connection.acknowledgedSequence = connection.readinessSequence
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
	if err != nil || !state.RelayPaused || !state.UserPaused || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
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
	resumeOptions.ReconnectEvery = 10 * time.Millisecond
	resumedConnections := make(chan *gatewayFake, 4)
	resumeOptions.NewGateway = func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
		connection := &gatewayFake{closed: make(chan struct{})}
		resumedConnections <- connection
		return connection, nil
	}
	resumeController, err = New(resumeOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeController.Resume(context.Background()); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	resumedConnection := <-resumedConnections
	resumedConnection.Close()
	select {
	case <-resumedConnections:
	case <-time.After(2 * time.Second):
		resumeController.mu.Lock()
		connected := resumeController.connection != nil && resumeController.connection.Status().Connected
		reconnectStarted := resumeController.reconnectStarted
		paused := resumeController.paused
		resumeController.mu.Unlock()
		service.mu.Lock()
		active := service.active
		service.mu.Unlock()
		t.Fatalf("resumed controller did not reconnect after a Gateway disconnect (connected=%t reconnect=%t paused=%t active=%t)", connected, reconnectStarted, paused, active)
	}
	if !resumeController.Close(context.Background()) {
		t.Fatal("resumed controller Close() failed")
	}
	if paused, err := preference.Load(testOrigin); err != nil || paused {
		t.Fatalf("cleared pause = %t, %v", paused, err)
	}
}

func TestRepairKeepsRuntimeFencedWhenThePinnedDriverCannotBeRestored(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	repairErr := errors.New("managed Cua files are foreign")
	runtime := &runtimeFake{state: cuaruntime.State{UpgradeRequired: true}, repairErr: repairErr}
	service := &installServiceFake{active: true}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	state, err := controller.Repair(context.Background())
	if !errors.Is(err, repairErr) || !state.UserPaused || !state.RelayPaused || !state.CuaUpgradeRequired {
		t.Fatalf("Repair() state=%#v err=%v", state, err)
	}
	service.mu.Lock()
	readiness := append([]apicontract.DesktopControlReadiness(nil), service.readiness...)
	service.mu.Unlock()
	if !preference.paused || len(readiness) != 1 || readiness[0] != apicontract.DesktopControlReadinessPaused || runtime.repairCalls != 1 {
		t.Fatalf("repair failure lost its safe state: saved=%t readiness=%v repairCalls=%d", preference.paused, readiness, runtime.repairCalls)
	}
}

func TestRepairRestoresAnPreviouslyActiveRelay(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true, active: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	state, err := controller.Repair(context.Background())
	if err != nil || !state.CuaReady || !state.NativeExecutorReady || !state.GatewayConnected || state.RelayPaused || state.UserPaused {
		t.Fatalf("Repair() state=%#v err=%v", state, err)
	}
	if preference.paused || runtime.repairCalls != 1 || connection.Status().Readiness != string(apicontract.DesktopControlReadinessReady) {
		t.Fatalf("repair did not restore prior active state: saved=%t repairCalls=%d readiness=%q", preference.paused, runtime.repairCalls, connection.Status().Readiness)
	}
}

func TestRepairFailureOrInactiveStateCannotAutoResumeOnReconnect(t *testing.T) {
	t.Parallel()
	statusFailure := errors.New("relay status unavailable")
	stateReadFailure := errors.New("local state unavailable")
	for _, test := range []struct {
		name       string
		active     bool
		statusErr  error
		lockState  desktopexecutor.SessionLockState
		lockErr    error
		stateErr   error
		wantRepair bool
		userPaused bool
	}{
		{name: "status failure", active: true, statusErr: statusFailure, lockState: desktopexecutor.SessionLockUnlocked, wantRepair: true, userPaused: true},
		{name: "inactive relay", active: false, lockState: desktopexecutor.SessionLockUnlocked},
		{name: "lock probe failure", active: true, lockState: desktopexecutor.SessionLockUnknown, lockErr: errors.New("lock probe failed"), userPaused: true},
		{name: "resume failure", active: true, lockState: desktopexecutor.SessionLockUnlocked, stateErr: stateReadFailure, wantRepair: true, userPaused: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			preference := &pausePreferenceFake{}
			service := &installServiceFake{active: test.active, statusErr: test.statusErr, localStateErr: test.stateErr}
			runtime := &runtimeFake{}
			controller, err := New(Options{
				Origin: testOrigin, Runtime: runtime, Installations: service,
				LockProbe: lockProbeFake{state: test.lockState, err: test.lockErr}, Local: localOperationsFake{}, PausePreference: preference,
				NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
				NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
					return &gatewayFake{closed: make(chan struct{})}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { controller.Close(context.Background()) })
			_, repairErr := controller.Repair(context.Background())
			if (repairErr != nil) != test.wantRepair {
				t.Fatalf("Repair() error = %v, want error=%t", repairErr, test.wantRepair)
			}
			if preference.paused != test.userPaused {
				t.Fatalf("saved pause = %t, want %t", preference.paused, test.userPaused)
			}
			controller.mu.Lock()
			userPaused, paused := controller.userPaused, controller.paused
			controller.mu.Unlock()
			if userPaused != test.userPaused || !paused {
				t.Fatalf("repair result lost its pause fence: userPaused=%t paused=%t", userPaused, paused)
			}
			runtime.mu.Lock()
			lastEvent := runtime.events[len(runtime.events)-1]
			runtime.mu.Unlock()
			if lastEvent != "stop" {
				t.Fatalf("last runtime action = %q, want stop while repair remains paused", lastEvent)
			}
			service.mu.Lock()
			service.statusErr = nil
			service.active = true
			service.localStateErr = nil
			service.mu.Unlock()
			if !test.userPaused {
				if !controller.Close(context.Background()) {
					t.Fatal("close controller before simulated restart")
				}
				restartedRuntime := &runtimeFake{}
				restarted, err := New(Options{
					Origin: testOrigin, Runtime: restartedRuntime, Installations: service,
					LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
					NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { restarted.Close(context.Background()) })
				if err := restarted.Recover(context.Background()); err != nil {
					t.Fatalf("Recover() after inactive repair = %v", err)
				}
				restarted.mu.Lock()
				restartedPaused, restartedUserPaused := restarted.paused, restarted.userPaused
				restarted.mu.Unlock()
				restartedRuntime.mu.Lock()
				prepareCalls := restartedRuntime.prepareCalls
				restartedRuntime.mu.Unlock()
				if restartedPaused || restartedUserPaused || prepareCalls != 1 {
					t.Fatalf("inactive repair blocked restart recovery: paused=%t userPaused=%t prepareCalls=%d", restartedPaused, restartedUserPaused, prepareCalls)
				}
				return
			}
			if err := controller.resumeIdleRelay(context.Background()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("reconnect resume = %v, want explicit retry required", err)
			}
		})
	}
}

func TestRepairStopsCuaWhenClearingLockedPauseFails(t *testing.T) {
	t.Parallel()
	pauseErr := errors.New("pause preference write failed")
	preference := &pausePreferenceFake{saveFalseErr: pauseErr}
	runtime := &runtimeFake{}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: runtime, Installations: &installServiceFake{active: true},
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockLocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	_, err = controller.Repair(context.Background())
	if !errors.Is(err, pauseErr) {
		t.Fatalf("Repair() error = %v, want pause persistence error", err)
	}
	runtime.mu.Lock()
	lastEvent := runtime.events[len(runtime.events)-1]
	runtime.mu.Unlock()
	if lastEvent != "stop" {
		t.Fatalf("last runtime action = %q, want stop after pause persistence failure", lastEvent)
	}
	if !preference.paused {
		t.Fatal("failed pause clear changed the persisted pause")
	}
}

func TestRepairRestartsActiveRelayAfterLockedSessionUnlocks(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	probe := &mutableLockProbe{state: desktopexecutor.SessionLockLocked}
	runtime := &runtimeFake{}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: runtime, Installations: &installServiceFake{active: true},
		LockProbe: probe, Local: localOperationsFake{}, PausePreference: preference, ReconnectEvery: 10 * time.Millisecond,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	state, err := controller.Repair(context.Background())
	if err != nil || state.UserPaused || !state.RelayPaused || preference.paused {
		t.Fatalf("Repair() while locked state=%#v savedPause=%t err=%v", state, preference.paused, err)
	}
	probe.Set(desktopexecutor.SessionLockUnlocked)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		controller.mu.Lock()
		paused := controller.paused
		controller.mu.Unlock()
		if !paused {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("active relay stayed fenced after the session unlocked")
}

func TestRepairSerializesPauseUntilItsResumeCompletes(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() before Repair() = %v", err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	runtime.mu.Lock()
	runtime.prepareBlockCall = runtime.prepareCalls + 1
	runtime.prepareStarted = started
	runtime.prepareRelease = release
	runtime.mu.Unlock()
	repairDone := make(chan error, 1)
	go func() {
		_, err := controller.Repair(context.Background())
		repairDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Repair() did not reach its serialized Resume()")
	}
	pauseDone := make(chan error, 1)
	go func() {
		_, err := controller.Pause(context.Background())
		pauseDone <- err
	}()
	select {
	case err := <-pauseDone:
		t.Fatalf("Pause() returned before serialized repair resume finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-repairDone; err != nil {
		t.Fatalf("Repair() = %v", err)
	}
	if err := <-pauseDone; err != nil {
		t.Fatalf("Pause() after Repair() = %v", err)
	}
	controller.mu.Lock()
	userPaused, paused := controller.userPaused, controller.paused
	controller.mu.Unlock()
	if !preference.paused || !userPaused || !paused {
		t.Fatalf("Pause() was lost across Repair(): saved=%t userPaused=%t paused=%t", preference.paused, userPaused, paused)
	}
}

func TestDisconnectRevokesInstallationAndStopsLocalGateway(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true}
	var connections []*gatewayFake
	var connectionsMu sync.Mutex
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			connection := &gatewayFake{closed: make(chan struct{})}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	state, err := controller.Disconnect(context.Background())
	if err != nil {
		t.Fatalf("Disconnect() = %v", err)
	}
	if state.InstallationID != nil || !state.RelayPaused || state.UserPaused || state.GatewayConnected {
		t.Fatalf("Disconnect() state = %#v", state)
	}
	service.mu.Lock()
	revoked, missing := service.revoked, service.missing
	service.mu.Unlock()
	if revoked != 1 || !missing {
		t.Fatalf("revoked=%d credential missing=%t, want one revoke and deleted credential", revoked, missing)
	}
	connectionsMu.Lock()
	firstConnection := connections[0]
	connectionsMu.Unlock()
	if firstConnection.Status().Connected {
		t.Fatal("Gateway remained connected after successful revoke")
	}
	if paused, err := preference.Load(testOrigin); err != nil || paused {
		t.Fatalf("pause preference after disconnect = %t, %v; want cleared", paused, err)
	}
	state, err = controller.Prepare(context.Background(), testOrigin, "ticket-again")
	connectionsMu.Lock()
	connectionCount := len(connections)
	var secondConnection *gatewayFake
	if connectionCount > 1 {
		secondConnection = connections[1]
	}
	connectionsMu.Unlock()
	if err != nil || state.InstallationID == nil || state.RelayPaused || !state.GatewayConnected || connectionCount != 2 || secondConnection == nil || !secondConnection.Status().Connected {
		t.Fatalf("fresh setup after disconnect state=%#v connections=%d err=%v", state, connectionCount, err)
	}
	service.mu.Lock()
	enrolled := service.enrolled
	service.mu.Unlock()
	if enrolled != 2 {
		t.Fatalf("fresh setup enrolled %d times, want two", enrolled)
	}
}

func TestDisconnectFailureKeepsCredentialAndGatewayPausedForRetry(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	revokeErr := errors.New("API revoke failed")
	service := &installServiceFake{missing: true, revokeErr: revokeErr}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	state, err := controller.Disconnect(context.Background())
	if !errors.Is(err, revokeErr) || !state.RelayPaused || !state.UserPaused {
		t.Fatalf("failed Disconnect() state=%#v err=%v", state, err)
	}
	service.mu.Lock()
	stillStored := !service.missing
	service.revokeErr = nil
	service.mu.Unlock()
	if !stillStored || !connection.Status().Connected || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("failed revoke lost retry state: credentialStored=%t gateway=%#v", stillStored, connection.Status())
	}
	if _, err := controller.Disconnect(context.Background()); err != nil {
		t.Fatalf("retry Disconnect() = %v", err)
	}
	service.mu.Lock()
	revoked, missing := service.revoked, service.missing
	service.mu.Unlock()
	if revoked != 2 || !missing || connection.Status().Connected {
		t.Fatalf("retry result revoked=%d missing=%t gateway=%#v", revoked, missing, connection.Status())
	}
}

func TestDisconnectKeyringFailureStillFencesAndPausesLocalExecution(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	keyringErr := errors.New("Secret Service unavailable")
	service.mu.Lock()
	service.storedErr = keyringErr
	service.mu.Unlock()
	state, err := controller.Disconnect(context.Background())
	if !errors.Is(err, keyringErr) || !state.RelayPaused || !state.UserPaused {
		t.Fatalf("Disconnect() with unavailable keyring state=%#v err=%v", state, err)
	}
	service.mu.Lock()
	revoked := service.revoked
	service.mu.Unlock()
	if revoked != 0 || !connection.Status().Connected || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("keyring failure lost paused retry state: revoked=%d gateway=%#v", revoked, connection.Status())
	}
	if paused, err := preference.Load(testOrigin); err != nil || !paused {
		t.Fatalf("saved pause after keyring failure = %t, %v", paused, err)
	}
}

func TestDisconnectCleanupFailureReportsPauseAndDoesNotRevoke(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	local := &retryCloseLocalOperations{}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: local,
		PausePreference: &pausePreferenceFake{},
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	state, err := controller.Disconnect(context.Background())
	if !errors.Is(err, ErrUnavailable) || !state.RelayPaused || !state.UserPaused {
		t.Fatalf("Disconnect() cleanup failure state=%#v err=%v", state, err)
	}
	service.mu.Lock()
	revoked, lastReadiness := service.revoked, service.readiness[len(service.readiness)-1]
	service.mu.Unlock()
	if revoked != 0 || lastReadiness != apicontract.DesktopControlReadinessPaused || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("cleanup failure crossed revoke fence: revoked=%d readiness=%v gateway=%#v", revoked, lastReadiness, connection.Status())
	}
}

func TestDisconnectWithMissingCredentialClosesGatewayAndAllowsFreshSetup(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true}
	var connections []*gatewayFake
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			connection := &gatewayFake{closed: make(chan struct{})}
			connections = append(connections, connection)
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	service.mu.Lock()
	service.missing = true
	service.mu.Unlock()
	state, err := controller.Disconnect(context.Background())
	if !errors.Is(err, credentialstore.ErrCredentialMissing) || state.InstallationID != nil || state.UserPaused || !state.RelayPaused {
		t.Fatalf("Disconnect() without credential state=%#v err=%v", state, err)
	}
	if connections[0].Status().Connected {
		t.Fatal("stale Gateway remained connected without its local credential")
	}
	if paused, err := preference.Load(testOrigin); err != nil || paused {
		t.Fatalf("pause preference after missing credential = %t, %v", paused, err)
	}
	state, err = controller.Prepare(context.Background(), testOrigin, "fresh-ticket")
	if err != nil || state.InstallationID == nil || state.UserPaused || !state.GatewayConnected || len(connections) != 2 {
		t.Fatalf("fresh setup after missing credential state=%#v connections=%d err=%v", state, len(connections), err)
	}
	service.mu.Lock()
	revoked, enrolled := service.revoked, service.enrolled
	service.mu.Unlock()
	if revoked != 0 || enrolled != 2 {
		t.Fatalf("missing-credential cleanup revoked=%d enrolled=%d, want local cleanup and fresh enrollment", revoked, enrolled)
	}
}

func TestDisconnectPreferenceFailureStillFencesBeforeReturning(t *testing.T) {
	t.Parallel()
	preferenceErr := errors.New("pause preference is read-only")
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{missing: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	preference.err = preferenceErr
	state, err := controller.Disconnect(context.Background())
	if !errors.Is(err, preferenceErr) || !state.UserPaused || !state.RelayPaused || connection.Status().Readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("Disconnect() preference failure state=%#v gateway=%#v err=%v", state, connection.Status(), err)
	}
	runtime.mu.Lock()
	stopCalls := runtime.stopCalls
	runtime.mu.Unlock()
	service.mu.Lock()
	revoked := service.revoked
	service.mu.Unlock()
	if stopCalls == 0 || revoked != 0 {
		t.Fatalf("preference failure did not fence before return: stop=%d revoke=%d", stopCalls, revoked)
	}
}

func TestReconnectAttemptCannotReuseInstallationAfterDisconnectAndReenroll(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	reconnectStatusStarted := make(chan struct{})
	reconnectStatusRelease := make(chan struct{})
	service := &installServiceFake{
		missing:       true,
		statusStarted: reconnectStatusStarted,
		statusRelease: reconnectStatusRelease,
	}
	var connectionsMu sync.Mutex
	connections := 0
	options := Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			connectionsMu.Lock()
			connections++
			connectionsMu.Unlock()
			return &gatewayFake{closed: make(chan struct{})}, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Prepare(context.Background(), testOrigin, "first-ticket"); err != nil {
		t.Fatalf("initial Prepare() = %v", err)
	}
	select {
	case <-reconnectStatusStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect loop did not capture old installation")
	}
	service.mu.Lock()
	oldInstallation := service.installation
	service.mu.Unlock()
	service.mu.Lock()
	service.missing = true
	service.mu.Unlock()
	if _, err := controller.Disconnect(context.Background()); !errors.Is(err, credentialstore.ErrCredentialMissing) {
		t.Fatalf("Disconnect() with missing credential = %v", err)
	}
	state, err := controller.Prepare(context.Background(), testOrigin, "second-ticket")
	if err != nil || state.InstallationID == nil || *state.InstallationID != "install_02" {
		t.Fatalf("fresh Prepare() state=%#v err=%v", state, err)
	}
	if err := controller.reconcileGateway(context.Background(), oldInstallation); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale reconnect attempt = %v, want ErrUnavailable", err)
	}
	close(reconnectStatusRelease)
	connectionsMu.Lock()
	createdConnections := connections
	connectionsMu.Unlock()
	if createdConnections != 2 {
		t.Fatalf("stale attempt created another Gateway: got %d connections, want 2", createdConnections)
	}
}

func TestRecoverClearsSavedPauseAfterInstallationCredentialWasRemoved(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{paused: true}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: &runtimeFake{}, Installations: &installServiceFake{missing: true},
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if err := controller.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() = %v", err)
	}
	controller.mu.Lock()
	userPaused, recoveryChecked := controller.userPaused, controller.recoveryChecked
	controller.mu.Unlock()
	if preference.paused || userPaused || !recoveryChecked {
		t.Fatalf("stale pause remained after credential removal: preference=%t userPaused=%t recoveryChecked=%t", preference.paused, userPaused, recoveryChecked)
	}
}

func TestLockMonitorCannotUndoUserPause(t *testing.T) {
	t.Parallel()
	controller := newTestController(t, &runtimeFake{}, &installServiceFake{}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(&runtimeFake{}, localOperationsFake{})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.userPaused = true
	controller.paused = true
	controller.currentLock = desktopexecutor.SessionLockUnknown
	controller.mu.Unlock()
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("lock monitor stopped after user pause")
	}
	if executor.NativeReady() {
		t.Fatal("stale lock monitor callback reopened a user-paused executor")
	}
	controller.mu.Lock()
	currentLock := controller.currentLock
	controller.mu.Unlock()
	if currentLock != desktopexecutor.SessionLockUnknown {
		t.Fatalf("current lock state = %q, want unknown while paused", currentLock)
	}
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockLocked) {
		t.Fatal("lock monitor stopped after locked callback during user pause")
	}
	controller.mu.Lock()
	currentLock = controller.currentLock
	controller.mu.Unlock()
	if currentLock != desktopexecutor.SessionLockLocked || executor.NativeReady() {
		t.Fatalf("locked callback during user pause left lock=%q native-ready=%t", currentLock, executor.NativeReady())
	}
	if !controller.Close(context.Background()) {
		t.Fatal("controller Close() failed")
	}
}

func TestLockMonitorCannotUndoPauseRestoredAfterResumeFailure(t *testing.T) {
	t.Parallel()
	controller := newTestController(t, &runtimeFake{}, &installServiceFake{}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(&runtimeFake{}, localOperationsFake{})
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.userPaused = false
	controller.paused = false
	controller.mu.Unlock()
	controller.restoreUserPause(context.Background())
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("lock monitor stopped after restoring the saved pause")
	}
	if executor.NativeReady() {
		t.Fatal("stale lock monitor callback reopened an executor after Resume failed")
	}
	controller.mu.Lock()
	userPaused := controller.userPaused
	controller.mu.Unlock()
	if !userPaused {
		t.Fatal("failed Resume did not restore the saved pause")
	}
	if !controller.Close(context.Background()) {
		t.Fatal("controller Close() failed")
	}
}

func TestLockMonitorCannotUndoGatewayDisconnectFence(t *testing.T) {
	t.Parallel()
	controller := newTestController(t, &runtimeFake{}, &installServiceFake{active: true}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(&runtimeFake{}, localOperationsFake{})
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
	controller.paused = false
	controller.mu.Unlock()
	connection.Close()
	controller.watchGateway(connection)
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("lock monitor stopped after Gateway disconnect")
	}
	if executor.NativeReady() {
		t.Fatal("lock monitor reopened execution after Gateway disconnect")
	}
	if !controller.Close(context.Background()) {
		t.Fatal("controller Close() failed")
	}
}

func TestLockMonitorCannotUndoIdleRelayFence(t *testing.T) {
	t.Parallel()
	controller := newTestController(t, &runtimeFake{}, &installServiceFake{active: false}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(&runtimeFake{}, localOperationsFake{})
	if err != nil {
		t.Fatal(err)
	}
	if !executor.SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("could not unlock test executor")
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.currentLock = desktopexecutor.SessionLockUnlocked
	controller.paused = false
	controller.mu.Unlock()
	controller.stopIdleRelay()
	if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("lock monitor stopped after idle relay cleanup")
	}
	if executor.NativeReady() {
		t.Fatal("lock monitor reopened execution after idle cleanup")
	}
	if !controller.Close(context.Background()) {
		t.Fatal("controller Close() failed")
	}
}

func TestHostedLocalStateRemainsAvailableWhenAPIStatusIsUnavailable(t *testing.T) {
	t.Parallel()
	service := &installServiceFake{statusErr: errors.New("API unavailable"), localState: installation.LocalState{RelayPaused: true}}
	controller := newTestController(t, &runtimeFake{}, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	t.Cleanup(func() { controller.Close(context.Background()) })
	state, err := controller.LocalState(context.Background(), testOrigin)
	if err != nil || !state.RelayPaused {
		t.Fatalf("hosted local state = %#v, %v", state, err)
	}
	state, err = controller.State(context.Background())
	if err != nil || !state.RelayPaused || state.RelayActive != nil {
		t.Fatalf("hosted controller state = %#v, %v", state, err)
	}
	state, err = controller.LifecycleState(context.Background(), testOrigin)
	if err != nil || state.RelayActive != nil {
		t.Fatalf("tray lifecycle state = %#v, %v, want local status and unknown relay activity", state, err)
	}
}

func TestReadinessWaitRequiresTheMatchingSentHeartbeat(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	controller := newTestController(t, runtime, &installServiceFake{}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	t.Cleanup(func() { controller.Close(context.Background()) })
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, localOperationsFake{})
	if err != nil {
		t.Fatal(err)
	}
	if !executor.SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("could not unlock test executor")
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.currentLock = desktopexecutor.SessionLockUnlocked
	controller.mu.Unlock()
	baseline := uint64(1)
	connection := &gatewayFake{
		connected: true, readiness: string(apicontract.DesktopControlReadinessReady),
		readinessSequence: 2, acknowledgedSequence: 1, lastHeartbeat: time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = controller.waitForAcknowledgedReadiness(ctx, connection, baseline, string(apicontract.DesktopControlReadinessReady), false)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale heartbeat acknowledgement = %v, want wait timeout", err)
	}
	connection.mu.Lock()
	connection.acknowledgedSequence = 2
	connection.lastHeartbeat = time.Now()
	connection.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.waitForAcknowledgedReadiness(ctx, connection, baseline, string(apicontract.DesktopControlReadinessReady), false); err != nil {
		t.Fatalf("matching readiness acknowledgement = %v", err)
	}
}

func TestReadinessWaitUsesAFixedHeartbeatAcknowledgementTarget(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	controller := newTestController(t, runtime, &installServiceFake{}, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, localOperationsFake{})
	if err != nil {
		t.Fatal(err)
	}
	if !executor.SetSessionLockState(context.Background(), desktopexecutor.SessionLockUnlocked) {
		t.Fatal("could not unlock test executor")
	}
	controller.mu.Lock()
	controller.executor = executor
	controller.currentLock = desktopexecutor.SessionLockUnlocked
	controller.mu.Unlock()
	connection := &advancingHeartbeatGateway{gatewayFake: &gatewayFake{
		connected: true, readiness: string(apicontract.DesktopControlReadinessReady), readinessSequence: 2,
		acknowledgedSequence: 1, lastHeartbeat: time.Now(),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.waitForAcknowledgedReadiness(ctx, connection, 1, string(apicontract.DesktopControlReadinessReady), false); err != nil {
		t.Fatalf("fixed readiness acknowledgement target did not complete: %v", err)
	}
}

func TestResumePassesABoundedOperationContextToDependencies(t *testing.T) {
	t.Parallel()
	service := &installServiceFake{}
	controller := newTestController(t, &runtimeFake{}, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, nil)
	controller.mu.Lock()
	controller.userPaused = true
	controller.mu.Unlock()
	_, err := controller.Resume(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Resume() error = %v, want inactive relay rejection", err)
	}
	service.mu.Lock()
	deadline := service.statusDeadline
	service.mu.Unlock()
	remaining := time.Until(deadline)
	if remaining < resumeOperationTimeout-time.Second || remaining > resumeOperationTimeout {
		t.Fatalf("Resume dependency deadline remaining = %s, want close to %s", remaining, resumeOperationTimeout)
	}
}

type advancingHeartbeatGateway struct{ *gatewayFake }

func (connection *advancingHeartbeatGateway) Status() desktopgateway.Status {
	connection.gatewayFake.mu.Lock()
	defer connection.gatewayFake.mu.Unlock()
	connection.gatewayFake.readinessSequence++
	connection.gatewayFake.acknowledgedSequence = connection.gatewayFake.readinessSequence - 1
	return desktopgateway.Status{
		Connected: connection.gatewayFake.connected, Readiness: connection.gatewayFake.readiness,
		ReadinessSequence:             connection.gatewayFake.readinessSequence,
		AcknowledgedHeartbeatSequence: connection.gatewayFake.acknowledgedSequence,
		LastHeartbeatAt:               connection.gatewayFake.lastHeartbeat,
	}
}

func TestPauseWorksWhenLocalRuntimeIsDegraded(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: strings.Repeat("A", 43), EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://cluster-agent.personastack.ai/v1/desktop-control/ws",
	}}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	state, err := controller.Pause(context.Background())
	if err != nil || !state.UserPaused || !state.RelayPaused {
		t.Fatalf("Pause() with degraded local runtime = %#v, %v", state, err)
	}
	if !preference.paused {
		t.Fatal("Pause() did not persist user intent")
	}
	service.mu.Lock()
	readiness := append([]apicontract.DesktopControlReadiness(nil), service.readiness...)
	service.mu.Unlock()
	if len(readiness) != 1 || readiness[0] != apicontract.DesktopControlReadinessPaused {
		t.Fatalf("readiness reports = %v, want paused", readiness)
	}
}

func TestPauseFencesLiveGatewayWhenAPIStatusIsUnavailable(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{}
	service := &installServiceFake{statusErr: errors.New("API status unavailable"), installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: strings.Repeat("A", 43), EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://cluster-agent.personastack.ai/v1/desktop-control/ws",
	}}
	runtime := &runtimeFake{}
	controller, err := New(Options{
		Origin: testOrigin, Runtime: runtime, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	executor, err := desktopexecutor.NewWithLocalOperations(runtime, localOperationsFake{})
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
	controller.paused = false
	controller.mu.Unlock()
	state, err := controller.Pause(context.Background())
	if err != nil || !state.UserPaused || state.RelayActive != nil {
		t.Fatalf("Pause() during API status outage = %#v, %v", state, err)
	}
	if !preference.paused || executor.NativeReady() {
		t.Fatalf("local pause was not enforced: saved=%t native-ready=%t", preference.paused, executor.NativeReady())
	}
	if readiness := connection.Status().Readiness; readiness != string(apicontract.DesktopControlReadinessPaused) {
		t.Fatalf("live Gateway readiness = %q, want paused", readiness)
	}
}

func TestResumeRechecksLockBeforeOpeningCommandAdmission(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{paused: true}
	service := &installServiceFake{active: true}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: &runtimeFake{}, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{},
		PausePreference: preference,
		NewLockMonitor:  func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	connection.onReadiness = func(string) {
		if !(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockLocked) {
			t.Error("lock monitor stopped during resume")
		}
	}
	if _, err := controller.Resume(context.Background()); !errors.Is(err, ErrLocked) {
		t.Fatalf("Resume() after a lock transition = %v, want locked", err)
	}
	if !preference.paused {
		t.Fatal("failed resume cleared the saved pause")
	}
	controller.mu.Lock()
	userPaused, paused := controller.userPaused, controller.paused
	controller.mu.Unlock()
	if !userPaused || !paused {
		t.Fatalf("failed resume opened command admission: userPaused=%t paused=%t", userPaused, paused)
	}
}

func TestResumeKeepsPauseWhenFinalStateReadFails(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{paused: true}
	service := &installServiceFake{active: true, localStateErr: errors.New("keyring read failed")}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: &runtimeFake{}, Installations: service,
		LockProbe: lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	if _, err := controller.Resume(context.Background()); err == nil {
		t.Fatal("Resume() succeeded despite its final local state read failing")
	}
	if !preference.paused {
		t.Fatal("failed Resume cleared the saved pause")
	}
	controller.mu.Lock()
	userPaused, paused := controller.userPaused, controller.paused
	executor := controller.executor
	controller.mu.Unlock()
	if !userPaused || !paused || executor == nil || executor.NativeReady() {
		t.Fatalf("failed Resume opened command admission: userPaused=%t paused=%t executor=%v", userPaused, paused, executor)
	}
}

func TestResumeDoesNotHoldLockFenceDuringSecretServiceRead(t *testing.T) {
	t.Parallel()
	preference := &pausePreferenceFake{paused: true}
	service := &installServiceFake{active: true, localStateStarted: make(chan struct{}), localStateRelease: make(chan struct{})}
	probe := &mutableLockProbe{state: desktopexecutor.SessionLockUnlocked}
	connection := &gatewayFake{closed: make(chan struct{})}
	options := Options{
		Origin: testOrigin, Runtime: &runtimeFake{}, Installations: service,
		LockProbe: probe, Local: localOperationsFake{}, PausePreference: preference,
		NewLockMonitor: func(LockStateSink, time.Duration) (LockMonitor, error) { return lockMonitorIdle{}, nil },
		NewGateway: func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error) {
			return connection, nil
		},
	}
	controller, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close(context.Background()) })
	resumeResult := make(chan error, 1)
	go func() {
		_, err := controller.Resume(context.Background())
		resumeResult <- err
	}()
	select {
	case <-service.localStateStarted:
	case <-time.After(time.Second):
		t.Fatal("Resume did not reach its local credential read")
	}
	probe.Set(desktopexecutor.SessionLockLocked)
	lockApplied := make(chan struct{})
	go func() {
		(lockSink{controller: controller}).SetSessionLockState(context.Background(), desktopexecutor.SessionLockLocked)
		close(lockApplied)
	}()
	select {
	case <-lockApplied:
	case <-time.After(time.Second):
		close(service.localStateRelease)
		t.Fatal("lock monitor was blocked by the local credential read")
	}
	close(service.localStateRelease)
	if err := <-resumeResult; !errors.Is(err, ErrLocked) {
		t.Fatalf("Resume() after lock during credential read = %v, want locked", err)
	}
	if !preference.paused {
		t.Fatal("lock transition during credential read cleared the saved pause")
	}
}

func TestPrepareConnectsThroughGatewayOwnedSessionLifecycle(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	service := &installServiceFake{active: true, statusErr: errors.New("API status unavailable"), record: make(chan string, 4), claimReady: make(chan struct{}), installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{}), record: service.record}
	var handler desktopgateway.CommandHandler
	controller := newTestController(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, func(_ desktopcontrol.Installation, _ string, commandHandler desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		handler = commandHandler
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
	controller.mu.Lock()
	controller.userPaused = true
	controller.paused = true
	controller.mu.Unlock()
	frame := agentgatewayruntime.DesktopControlFrame{
		Version: agentgatewayruntime.DesktopControlProtocolVersion,
		Type:    agentgatewayruntime.DesktopControlFrameCommand, RequestID: "resume-pending",
		Target:    &agentgatewayruntime.DesktopControlTarget{InstallationID: service.installation.InstallationID, WorkspaceID: "workspace_1", ConfigID: "config_1", PersonaID: "persona_1", RunID: "run_1", Generation: 1},
		Operation: agentgatewayruntime.DesktopControlOperationStatus, Arguments: json.RawMessage(`{}`), DeadlineAt: time.Now().Add(time.Minute),
	}
	response := handler(context.Background(), frame, func(agentgatewayruntime.DesktopControlFrame) error { return nil })
	if response.Type != agentgatewayruntime.DesktopControlFrameFailure || response.ErrorCode != "desktop_executor_unavailable" {
		t.Fatalf("Gateway accepted work while lifecycle was paused: %#v", response)
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

func TestGatewayHandlerScopesRevocationAndDeniesUnleasedCallsWithoutEffects(t *testing.T) {
	t.Parallel()
	runtime := &runtimeFake{}
	local := &recordingLocalOperations{}
	service := &installServiceFake{active: true, installation: desktopcontrol.Installation{
		InstallationID: "install_01", MachineCredential: "secret", EnvironmentOrigin: testOrigin,
		GatewayWebsocketURL: "wss://my.personastack.ai/v1/desktop-control/gateway",
	}}
	connection := &gatewayFake{closed: make(chan struct{})}
	var handler desktopgateway.CommandHandler
	controller := newTestControllerWithLocal(t, runtime, service, lockProbeFake{state: desktopexecutor.SessionLockUnlocked}, func(_ desktopcontrol.Installation, _ string, commandHandler desktopgateway.CommandHandler, _ desktopgateway.DiagnosticsProvider) (gateway, error) {
		handler = commandHandler
		return connection, nil
	}, local)
	if _, err := controller.Prepare(context.Background(), testOrigin, "ticket"); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	target := func(workspace, config string) *agentgatewayruntime.DesktopControlTarget {
		return &agentgatewayruntime.DesktopControlTarget{
			InstallationID: service.installation.InstallationID,
			WorkspaceID:    workspace, ConfigID: config, PersonaID: "persona_1", RunID: "run_1",
			Generation: 1, ConfigVersion: 1,
		}
	}
	requestID := 0
	command := func(target *agentgatewayruntime.DesktopControlTarget, operation agentgatewayruntime.DesktopControlOperation, arguments string) agentgatewayruntime.DesktopControlFrame {
		requestID++
		return agentgatewayruntime.DesktopControlFrame{
			Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameCommand,
			RequestID: fmt.Sprintf("request-%d", requestID), Target: target, Operation: operation,
			Arguments: json.RawMessage(arguments), DeadlineAt: time.Now().Add(time.Minute),
		}
	}
	dispatch := func(frame agentgatewayruntime.DesktopControlFrame) agentgatewayruntime.DesktopControlFrame {
		if err := agentgatewayruntime.ValidateDesktopControlFrame(frame); err != nil {
			t.Fatalf("test command frame %s/%s is invalid: %v", frame.RequestID, frame.Operation, err)
		}
		return handler(context.Background(), frame, func(agentgatewayruntime.DesktopControlFrame) error { return nil })
	}
	const missingToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	observe := func(token string) string {
		return `{"control_token":"` + token + `","tool":"get_desktop_state","arguments":{}}`
	}
	firstTarget := target("workspace_a", "config_a")
	denied := dispatch(command(firstTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(missingToken)))
	if denied.Type != agentgatewayruntime.DesktopControlFrameFailure || denied.ErrorCode != "desktop_control_required" || runtime.callCount() != 0 || local.closeCount() != 0 {
		t.Fatalf("unleased Gateway command = %#v, Cua calls = %d, local cleanups = %d", denied, runtime.callCount(), local.closeCount())
	}
	firstLease := dispatch(command(firstTarget, agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	if firstLease.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("workspace A lease = %#v", firstLease)
	}
	var firstPayload struct {
		ControlToken string `json:"control_token"`
	}
	if err := json.Unmarshal(firstLease.Result, &firstPayload); err != nil || firstPayload.ControlToken == "" {
		t.Fatalf("workspace A lease payload = %s, error = %v", firstLease.Result, err)
	}
	firstCall := dispatch(command(firstTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(firstPayload.ControlToken)))
	if firstCall.Type != agentgatewayruntime.DesktopControlFrameResult || runtime.callCount() != 1 || local.closeCount() != 0 {
		t.Fatalf("workspace A Cua call = %#v, Cua calls = %d, local cleanups = %d", firstCall, runtime.callCount(), local.closeCount())
	}
	revokedTarget := &agentgatewayruntime.DesktopControlTarget{
		InstallationID: firstTarget.InstallationID, WorkspaceID: firstTarget.WorkspaceID,
		ConfigID: firstTarget.ConfigID, ConfigVersion: 2,
	}
	revoke := dispatch(command(revokedTarget, agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`))
	if revoke.Type != agentgatewayruntime.DesktopControlFrameResult || string(revoke.Result) != `{"revoked":true}` || local.closeCount() != 1 {
		t.Fatalf("workspace A revoke = %#v, local cleanups = %d", revoke, local.closeCount())
	}
	denied = dispatch(command(firstTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(firstPayload.ControlToken)))
	if denied.Type != agentgatewayruntime.DesktopControlFrameFailure || runtime.callCount() != 1 || local.closeCount() != 1 {
		t.Fatalf("revoked workspace A call = %#v, Cua calls = %d, local cleanups = %d", denied, runtime.callCount(), local.closeCount())
	}
	siblingTarget := target("workspace_a", "config_b")
	siblingLease := dispatch(command(siblingTarget, agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	if siblingLease.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("sibling config lease after config A revoke = %#v", siblingLease)
	}
	var siblingPayload struct {
		ControlToken string `json:"control_token"`
	}
	if err := json.Unmarshal(siblingLease.Result, &siblingPayload); err != nil || siblingPayload.ControlToken == "" {
		t.Fatalf("sibling config lease payload = %s, error = %v", siblingLease.Result, err)
	}
	siblingCall := dispatch(command(siblingTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(siblingPayload.ControlToken)))
	if siblingCall.Type != agentgatewayruntime.DesktopControlFrameResult || runtime.callCount() != 2 {
		t.Fatalf("sibling config Cua call = %#v, Cua calls = %d", siblingCall, runtime.callCount())
	}
	lateRevoke := dispatch(command(revokedTarget, agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`))
	if lateRevoke.Type != agentgatewayruntime.DesktopControlFrameResult || local.closeCount() != 1 {
		t.Fatalf("delayed config A revoke with sibling lease active = %#v, local cleanups = %d", lateRevoke, local.closeCount())
	}
	siblingAfterRevoke := dispatch(command(siblingTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(siblingPayload.ControlToken)))
	if siblingAfterRevoke.Type != agentgatewayruntime.DesktopControlFrameResult || runtime.callCount() != 3 || local.closeCount() != 1 {
		t.Fatalf("sibling config call after config A revoke = %#v, Cua calls = %d, local cleanups = %d", siblingAfterRevoke, runtime.callCount(), local.closeCount())
	}
	siblingRelease := dispatch(command(siblingTarget, agentgatewayruntime.DesktopControlOperationRelease, `{"control_token":"`+siblingPayload.ControlToken+`"}`))
	if siblingRelease.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("sibling config lease release = %#v", siblingRelease)
	}
	if local.closeCount() != 2 {
		t.Fatalf("sibling config release local cleanups = %d, want 2", local.closeCount())
	}
	otherWorkspaceTarget := target("workspace_b", "config_a")
	otherWorkspaceLease := dispatch(command(otherWorkspaceTarget, agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	if otherWorkspaceLease.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("same config ID in another workspace lease = %#v", otherWorkspaceLease)
	}
	var otherWorkspacePayload struct {
		ControlToken string `json:"control_token"`
	}
	if err := json.Unmarshal(otherWorkspaceLease.Result, &otherWorkspacePayload); err != nil || otherWorkspacePayload.ControlToken == "" {
		t.Fatalf("other workspace lease payload = %s, error = %v", otherWorkspaceLease.Result, err)
	}
	lateRevoke = dispatch(command(revokedTarget, agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`))
	if lateRevoke.Type != agentgatewayruntime.DesktopControlFrameResult || local.closeCount() != 2 {
		t.Fatalf("delayed workspace A revoke = %#v, local cleanups = %d", lateRevoke, local.closeCount())
	}
	otherWorkspaceCall := dispatch(command(otherWorkspaceTarget, agentgatewayruntime.DesktopControlOperationObserve, observe(otherWorkspacePayload.ControlToken)))
	if otherWorkspaceCall.Type != agentgatewayruntime.DesktopControlFrameResult || runtime.callCount() != 4 || local.closeCount() != 2 {
		t.Fatalf("same config ID in another workspace after delayed revoke = %#v, Cua calls = %d, local cleanups = %d", otherWorkspaceCall, runtime.callCount(), local.closeCount())
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
	controller.paused = false
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
	controller.paused = false
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
	return newTestControllerWithLocal(t, runtime, service, probe, newGateway, localOperationsFake{})
}

func newTestControllerWithLocal(t *testing.T, runtime cuaRuntime, service *installServiceFake, probe lockProbe, newGateway gatewayFactory, local desktopexecutor.LocalOperations) *Controller {
	t.Helper()
	options := Options{Origin: testOrigin, Runtime: runtime, Installations: service, LockProbe: probe, Local: local, PausePreference: &pausePreferenceFake{}}
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
