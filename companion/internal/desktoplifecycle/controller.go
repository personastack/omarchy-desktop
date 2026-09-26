//go:build linux

package desktoplifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/cuaruntime"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopexecutor"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopgateway"
	"github.com/personastack/omarchy-desktop/companion/internal/hyprlandlock"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const (
	reconnectInterval      = 5 * time.Second
	resumeOperationTimeout = 11 * time.Minute
)

var (
	ErrUnavailable = errors.New("desktop control lifecycle unavailable")
	ErrLocked      = codedError{code: "session_locked", message: "unlock the Hyprland session before preparing Desktop Control"}
	ErrSession     = codedError{code: "session_state_unknown", message: "Hyprland session state is unknown"}
)

type codedError struct{ code, message string }

func (err codedError) Error() string                   { return err.message }
func (err codedError) DesktopControlErrorCode() string { return err.code }

type cuaRuntime interface {
	Prepare(context.Context) (cuaruntime.State, error)
	Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error)
	State() cuaruntime.State
	Stop()
	Close()
}

type installationService interface {
	StoredInstallation(context.Context, string) (desktopcontrol.Installation, error)
	Enroll(context.Context, string, apicontract.DesktopControlOperatingSystem) error
	Attach(context.Context, string, string) error
	ReportReadiness(context.Context, string, apicontract.DesktopControlReadiness) error
	Status(context.Context, string) (installation.Status, error)
	LocalState(context.Context, string) (installation.LocalState, error)
}

type gateway interface {
	ConnectWithLifetime(context.Context, context.Context) error
	Status() desktopgateway.Status
	SetReadiness(string) error
	Wait(context.Context) error
	Close()
}

type gatewayFactory func(desktopcontrol.Installation, string, desktopgateway.CommandHandler, desktopgateway.DiagnosticsProvider) (gateway, error)

type LockStateSink interface {
	SetSessionLockState(context.Context, desktopexecutor.SessionLockState) bool
}

type LockMonitor interface {
	Run(context.Context) error
}

type lockMonitorFactory func(LockStateSink, time.Duration) (LockMonitor, error)

// PausePreference keeps an explicit user pause separate from idle cleanup.
// The preference contains no credentials and is scoped by app origin.
type PausePreference interface {
	Load(string) (bool, error)
	Save(string, bool) error
}

type Options struct {
	Origin          string
	Runtime         cuaRuntime
	Installations   installationService
	LockProbe       lockProbe
	Local           desktopexecutor.LocalOperations
	NewGateway      gatewayFactory
	NewLockMonitor  lockMonitorFactory
	PausePreference PausePreference
	PollInterval    time.Duration
	ReconnectEvery  time.Duration
}

type lockProbe interface {
	State(context.Context) (desktopexecutor.SessionLockState, error)
}

type Controller struct {
	origin          string
	runtime         cuaRuntime
	installations   installationService
	probe           lockProbe
	local           desktopexecutor.LocalOperations
	newGateway      gatewayFactory
	newLockMonitor  lockMonitorFactory
	pausePreference PausePreference
	pollInterval    time.Duration
	reconnectEvery  time.Duration

	mu                      sync.Mutex
	lockStateMu             sync.Mutex
	readinessMu             sync.Mutex
	prepareGate             chan struct{}
	connectGate             chan struct{}
	closeGate               chan struct{}
	ctx                     context.Context
	cancel                  context.CancelFunc
	executor                *desktopexecutor.Executor
	monitorStop             context.CancelFunc
	connection              gateway
	currentLock             desktopexecutor.SessionLockState
	reconnectStarted        bool
	setupMayRunUnconfigured bool
	setupDeadline           time.Time
	recoveryChecked         bool
	closed                  bool
	paused                  bool
	userPaused              bool
	closeComplete           bool
	closeSuccess            bool
}

func New(options Options) (*Controller, error) {
	if options.Origin == "" || options.Runtime == nil || options.Installations == nil || options.LockProbe == nil || options.Local == nil {
		return nil, ErrUnavailable
	}
	if options.PollInterval == 0 {
		options.PollInterval = 500 * time.Millisecond
	}
	if options.ReconnectEvery == 0 {
		options.ReconnectEvery = reconnectInterval
	}
	if options.PollInterval < 500*time.Millisecond || options.PollInterval > 30*time.Second {
		return nil, ErrUnavailable
	}
	if options.ReconnectEvery < 10*time.Millisecond || options.ReconnectEvery > 30*time.Second {
		return nil, ErrUnavailable
	}
	if options.NewGateway == nil {
		options.NewGateway = defaultGatewayFactory
	}
	if options.PausePreference == nil {
		return nil, ErrUnavailable
	}
	userPaused, err := options.PausePreference.Load(options.Origin)
	if err != nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	monitorFactory := options.NewLockMonitor
	if monitorFactory == nil {
		probe, ok := options.LockProbe.(*hyprlandlock.Probe)
		if !ok {
			cancel()
			return nil, ErrUnavailable
		}
		monitorFactory = func(sink LockStateSink, interval time.Duration) (LockMonitor, error) {
			return &hyprlandlock.Monitor{Probe: probe, PollInterval: interval, Sink: sink}, nil
		}
	}
	return &Controller{
		origin: options.Origin, runtime: options.Runtime, installations: options.Installations,
		probe: options.LockProbe, local: options.Local, newGateway: options.NewGateway,
		newLockMonitor: monitorFactory, pollInterval: options.PollInterval, reconnectEvery: options.ReconnectEvery,
		pausePreference: options.PausePreference, userPaused: userPaused,
		prepareGate: make(chan struct{}, 1), connectGate: make(chan struct{}, 1),
		closeGate: make(chan struct{}, 1), ctx: ctx, cancel: cancel, paused: true,
		currentLock: desktopexecutor.SessionLockUnknown,
	}, nil
}

func defaultGatewayFactory(installation desktopcontrol.Installation, origin string, handler desktopgateway.CommandHandler, diagnostics desktopgateway.DiagnosticsProvider) (gateway, error) {
	return desktopgateway.New(installation, origin, handler, desktopgateway.Options{Diagnostics: diagnostics})
}

// Prepare validates the current compositor lock state, starts Cua, enrolls or
// attaches the installation, and starts the API-authorized reconnect loop.
func (controller *Controller) Prepare(ctx context.Context, origin, ticket string) (installation.LocalState, error) {
	if ctx == nil || origin != controller.origin || ticket == "" {
		return installation.LocalState{}, ErrUnavailable
	}
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-ctx.Done():
		return installation.LocalState{}, ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed {
		controller.mu.Unlock()
		return installation.LocalState{}, ErrUnavailable
	}
	userPaused := controller.userPaused
	controller.mu.Unlock()
	if userPaused {
		return installation.LocalState{}, ErrUnavailable
	}
	state, err := controller.probe.State(ctx)
	if err != nil || state == desktopexecutor.SessionLockUnknown {
		return installation.LocalState{}, ErrSession
	}
	if state == desktopexecutor.SessionLockLocked {
		return installation.LocalState{}, ErrLocked
	}
	if _, err := controller.runtime.Prepare(ctx); err != nil {
		return installation.LocalState{}, fmt.Errorf("prepare Linux Cua runtime: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return installation.LocalState{}, err
	}
	installed, err := controller.installations.StoredInstallation(ctx, controller.origin)
	existing := err == nil
	if errors.Is(err, credentialstore.ErrCredentialMissing) {
		if err := controller.installations.Enroll(ctx, ticket, apicontract.DesktopControlOperatingSystemLinux); err != nil {
			return installation.LocalState{}, err
		}
		installed, err = controller.installations.StoredInstallation(ctx, controller.origin)
	}
	if err != nil {
		return installation.LocalState{}, err
	}
	if err := ctx.Err(); err != nil {
		return installation.LocalState{}, err
	}
	if err := controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessReady); err != nil {
		return installation.LocalState{}, err
	}
	if err := ctx.Err(); err != nil {
		return installation.LocalState{}, err
	}
	if existing {
		if err := controller.installations.Attach(ctx, controller.origin, ticket); err != nil {
			return installation.LocalState{}, err
		}
		if err := ctx.Err(); err != nil {
			return installation.LocalState{}, err
		}
	}
	controller.mu.Lock()
	hadExecutor := controller.executor != nil
	controller.mu.Unlock()
	if err := controller.startExecutorAndMonitor(ctx); err != nil {
		return installation.LocalState{}, err
	}
	if err := ctx.Err(); err != nil {
		if !hadExecutor {
			controller.abortPrepare()
		}
		return installation.LocalState{}, err
	}
	controller.mu.Lock()
	controller.paused = false
	controller.setupMayRunUnconfigured = true
	controller.setupDeadline = time.Now().Add(10 * time.Minute)
	controller.recoveryChecked = true
	controller.mu.Unlock()
	connectErr := controller.reconcileGateway(ctx, installed)
	controller.startReconnectLoop(installed)
	if connectErr != nil {
		return installation.LocalState{}, connectErr
	}
	controller.mu.Lock()
	lockState := controller.currentLock
	controller.mu.Unlock()
	if lockState == desktopexecutor.SessionLockLocked {
		return installation.LocalState{}, ErrLocked
	}
	if lockState != desktopexecutor.SessionLockUnlocked {
		return installation.LocalState{}, ErrSession
	}
	return controller.State(ctx)
}

// Recover restores an enrolled installation after app restart when the API
// still has an active Desktop Control mapping. It never enrolls or attaches.
func (controller *Controller) Recover(ctx context.Context) error {
	if ctx == nil {
		return ErrUnavailable
	}
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed || controller.recoveryChecked || controller.executor != nil {
		controller.mu.Unlock()
		return nil
	}
	if controller.userPaused {
		controller.recoveryChecked = true
		controller.mu.Unlock()
		return nil
	}
	controller.mu.Unlock()
	installed, err := controller.installations.StoredInstallation(ctx, controller.origin)
	if err != nil {
		return err
	}
	status, err := controller.installations.Status(ctx, controller.origin)
	if err != nil || !status.RelayActive || !status.CredentialValid {
		controller.mu.Lock()
		if err == nil {
			controller.recoveryChecked = true
		}
		controller.mu.Unlock()
		if err == nil && status.CredentialValid {
			controller.startReconnectLoop(installed)
		}
		return err
	}
	state, err := controller.probe.State(ctx)
	if err != nil || state != desktopexecutor.SessionLockUnlocked {
		return ErrSession
	}
	if _, err := controller.runtime.Prepare(ctx); err != nil {
		return fmt.Errorf("prepare Linux Cua runtime: %w", err)
	}
	if err := controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessReady); err != nil {
		return err
	}
	if err := controller.startExecutorAndMonitor(ctx); err != nil {
		return err
	}
	controller.mu.Lock()
	controller.paused = false
	controller.recoveryChecked = true
	controller.mu.Unlock()
	controller.startReconnectLoop(installed)
	return nil
}

// StartRecovery retries startup restoration while the login session is locked
// or the API is temporarily unavailable. It stops after proving there is no
// active mapping or after restoring the local runtime.
func (controller *Controller) StartRecovery() {
	controller.mu.Lock()
	if controller.closed || controller.recoveryChecked {
		controller.mu.Unlock()
		return
	}
	controller.mu.Unlock()
	go func() {
		for controller.ctx.Err() == nil {
			err := controller.Recover(controller.ctx)
			if err == nil || errors.Is(err, credentialstore.ErrCredentialMissing) {
				return
			}
			if !wait(controller.ctx, controller.reconnectEvery) {
				return
			}
		}
	}()
}

func (controller *Controller) startExecutorAndMonitor(ctx context.Context) error {
	initial, err := controller.probe.State(ctx)
	if err != nil || initial == desktopexecutor.SessionLockUnknown {
		_ = controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessUnknown)
		return ErrSession
	}
	if initial == desktopexecutor.SessionLockLocked {
		_ = controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessLocked)
		return ErrLocked
	}
	controller.lockStateMu.Lock()
	defer controller.lockStateMu.Unlock()
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.closed {
		return ErrUnavailable
	}
	if controller.executor == nil {
		executor, err := desktopexecutor.NewWithLocalOperations(controller.runtime, controller.local)
		if err != nil {
			return err
		}
		controller.executor = executor
	}
	if !controller.executor.SetSessionLockState(controller.ctx, initial) {
		return ErrUnavailable
	}
	controller.currentLock = initial
	if controller.monitorStop != nil {
		return nil
	}
	monitorContext, stop := context.WithCancel(controller.ctx)
	controller.monitorStop = stop
	monitor, err := controller.newLockMonitor(lockSink{controller: controller}, controller.pollInterval)
	if err != nil {
		stop()
		controller.monitorStop = nil
		return err
	}
	go func() { _ = monitor.Run(monitorContext) }()
	return nil
}

type lockSink struct{ controller *Controller }

func (sink lockSink) SetSessionLockState(ctx context.Context, state desktopexecutor.SessionLockState) bool {
	controller := sink.controller
	if ctx == nil || ctx.Err() != nil {
		return true
	}
	controller.lockStateMu.Lock()
	defer controller.lockStateMu.Unlock()
	controller.mu.Lock()
	executor := controller.executor
	closed := controller.closed
	paused := controller.userPaused || controller.paused
	controller.mu.Unlock()
	if closed || (paused && state == desktopexecutor.SessionLockUnlocked) {
		return true
	}
	if executor != nil {
		_ = executor.SetSessionLockState(ctx, state)
	}
	controller.mu.Lock()
	controller.currentLock = state
	connection := controller.connection
	controller.mu.Unlock()
	controller.updateGatewayReadiness(connection)
	return true
}

func (controller *Controller) updateGatewayReadiness(connection gateway) {
	if connection == nil {
		return
	}
	controller.readinessMu.Lock()
	defer controller.readinessMu.Unlock()
	controller.mu.Lock()
	if controller.connection != connection {
		controller.mu.Unlock()
		return
	}
	lockState := controller.currentLock
	userPaused := controller.userPaused
	controller.mu.Unlock()
	readiness := readinessFor(lockState, controller.runtime.State().Ready && controller.nativeExecutorReady())
	if userPaused {
		readiness = string(apicontract.DesktopControlReadinessPaused)
	}
	if connection.Status().Readiness != readiness {
		_ = connection.SetReadiness(readiness)
	}
}

func readinessFor(lockState desktopexecutor.SessionLockState, cuaReady bool) string {
	switch lockState {
	case desktopexecutor.SessionLockLocked:
		return string(apicontract.DesktopControlReadinessLocked)
	case desktopexecutor.SessionLockUnknown:
		return string(apicontract.DesktopControlReadinessUnknown)
	default:
		if cuaReady {
			return string(apicontract.DesktopControlReadinessReady)
		}
		return string(apicontract.DesktopControlReadinessCuaUnavailable)
	}
}

func (controller *Controller) State(ctx context.Context) (installation.LocalState, error) {
	return controller.LocalState(ctx, controller.origin)
}

// Pause fences local execution, reports paused readiness, and stops the Cua
// runtime while retaining the enrolled installation for an explicit resume.
func (controller *Controller) Pause(ctx context.Context) (installation.LocalState, error) {
	if ctx == nil {
		return installation.LocalState{}, ErrUnavailable
	}
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-ctx.Done():
		return installation.LocalState{}, ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed {
		controller.mu.Unlock()
		return installation.LocalState{}, ErrUnavailable
	}
	if controller.userPaused {
		controller.mu.Unlock()
		return controller.LifecycleState(ctx, controller.origin)
	}
	connection := controller.connection
	controller.mu.Unlock()
	installed, err := controller.installations.StoredInstallation(ctx, controller.origin)
	if err != nil || installed.Validate(controller.origin) != nil {
		return installation.LocalState{}, ErrUnavailable
	}
	if err := controller.pausePreference.Save(controller.origin, true); err != nil {
		return installation.LocalState{}, err
	}
	controller.lockStateMu.Lock()
	controller.mu.Lock()
	controller.userPaused = true
	controller.paused = true
	stop := controller.monitorStop
	controller.monitorStop = nil
	executor := controller.executor
	controller.mu.Unlock()
	if executor != nil {
		_ = executor.SetSessionLockState(controller.ctx, desktopexecutor.SessionLockUnknown)
	}
	controller.lockStateMu.Unlock()
	if stop != nil {
		stop()
	}
	controller.runtime.Stop()
	if connection != nil && connection.Status().Connected {
		controller.readinessMu.Lock()
		_ = connection.SetReadiness(string(apicontract.DesktopControlReadinessPaused))
		controller.readinessMu.Unlock()
	}
	_ = controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessPaused)
	return controller.LocalState(ctx, controller.origin)
}

// Resume only restores execution from a saved user pause after confirming the
// compositor session is unlocked and the API still recognizes the install.
func (controller *Controller) Resume(ctx context.Context) (installation.LocalState, error) {
	if ctx == nil {
		return installation.LocalState{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, resumeOperationTimeout)
	defer cancel()
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-ctx.Done():
		return installation.LocalState{}, ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed {
		controller.mu.Unlock()
		return installation.LocalState{}, ErrUnavailable
	}
	if !controller.userPaused {
		controller.mu.Unlock()
		return controller.LifecycleState(ctx, controller.origin)
	}
	controller.mu.Unlock()
	status, err := controller.installations.Status(ctx, controller.origin)
	if err != nil || !status.RelayActive || !status.CredentialValid {
		return installation.LocalState{}, ErrUnavailable
	}
	installed, err := controller.installations.StoredInstallation(ctx, controller.origin)
	if err != nil {
		return installation.LocalState{}, err
	}
	lockState, err := controller.probe.State(ctx)
	if err != nil || lockState != desktopexecutor.SessionLockUnlocked {
		if lockState == desktopexecutor.SessionLockUnknown || err != nil {
			return installation.LocalState{}, ErrSession
		}
		return installation.LocalState{}, ErrLocked
	}
	if _, err := controller.runtime.Prepare(ctx); err != nil {
		return installation.LocalState{}, err
	}
	if err := controller.startExecutorAndMonitor(ctx); err != nil {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	if err := controller.reconcileGateway(ctx, installed); err != nil {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	controller.mu.Lock()
	connection := controller.connection
	lockState = controller.currentLock
	controller.mu.Unlock()
	if connection == nil || !connection.Status().Connected {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, ErrUnavailable
	}
	baseline := connection.Status().ReadinessSequence
	readiness := readinessFor(lockState, controller.runtime.State().Ready && controller.nativeExecutorReady())
	if err := connection.SetReadiness(readiness); err != nil {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	if err := controller.waitForAcknowledgedReadiness(ctx, connection, baseline, readiness, true); err != nil {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	if err := controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessReady); err != nil {
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	state, err := controller.LocalState(ctx, controller.origin)
	if err != nil || ctx.Err() != nil {
		controller.restoreUserPause(ctx)
		if err == nil {
			err = ctx.Err()
		}
		return installation.LocalState{}, err
	}
	controller.lockStateMu.Lock()
	confirmedLock, probeErr := controller.probe.State(ctx)
	if probeErr != nil || confirmedLock != desktopexecutor.SessionLockUnlocked {
		controller.lockStateMu.Unlock()
		controller.restoreUserPause(ctx)
		if probeErr != nil || confirmedLock == desktopexecutor.SessionLockUnknown {
			return installation.LocalState{}, ErrSession
		}
		return installation.LocalState{}, ErrLocked
	}
	controller.mu.Lock()
	executor := controller.executor
	connection = controller.connection
	closed := controller.closed
	controller.mu.Unlock()
	if closed || executor == nil || connection == nil || !connection.Status().Connected ||
		connection.Status().Readiness != string(apicontract.DesktopControlReadinessReady) ||
		!controller.runtime.State().Ready || !executor.NativeReady() {
		controller.lockStateMu.Unlock()
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, ErrUnavailable
	}
	if !executor.SetSessionLockState(ctx, desktopexecutor.SessionLockUnlocked) {
		controller.lockStateMu.Unlock()
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, ErrUnavailable
	}
	if err := controller.pausePreference.Save(controller.origin, false); err != nil {
		controller.lockStateMu.Unlock()
		controller.restoreUserPause(ctx)
		return installation.LocalState{}, err
	}
	controller.mu.Lock()
	controller.currentLock = confirmedLock
	controller.userPaused = false
	controller.paused = false
	controller.mu.Unlock()
	controller.lockStateMu.Unlock()
	controller.startReconnectLoop(installed)
	state.RelayActive = &status.RelayActive
	state.RelayPaused = false
	state.UserPaused = false
	return state, nil
}

func (controller *Controller) restoreUserPause(ctx context.Context) {
	_ = controller.pausePreference.Save(controller.origin, true)
	controller.lockStateMu.Lock()
	controller.mu.Lock()
	controller.userPaused = true
	controller.paused = true
	stop := controller.monitorStop
	controller.monitorStop = nil
	executor := controller.executor
	connection := controller.connection
	controller.mu.Unlock()
	if executor != nil {
		_ = executor.SetSessionLockState(controller.ctx, desktopexecutor.SessionLockUnknown)
	}
	controller.lockStateMu.Unlock()
	if stop != nil {
		stop()
	}
	controller.runtime.Stop()
	_ = controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessPaused)
	if connection != nil {
		controller.readinessMu.Lock()
		_ = connection.SetReadiness(string(apicontract.DesktopControlReadinessPaused))
		controller.readinessMu.Unlock()
	}
}

func (controller *Controller) LocalState(ctx context.Context, origin string) (installation.LocalState, error) {
	if ctx == nil || origin != controller.origin {
		return installation.LocalState{}, ErrUnavailable
	}
	state, err := controller.installations.LocalState(ctx, origin)
	if err != nil {
		return installation.LocalState{}, err
	}
	controller.mu.Lock()
	executor := controller.executor
	connection := controller.connection
	controller.mu.Unlock()
	state.CuaReady = controller.runtime.State().Ready
	state.NativeExecutorReady = executor != nil && executor.NativeReady()
	state.GatewayConnected = connection != nil && connection.Status().Connected
	controller.mu.Lock()
	state.RelayPaused = controller.paused
	state.UserPaused = controller.userPaused
	controller.mu.Unlock()
	return state, nil
}

func (controller *Controller) LifecycleState(ctx context.Context, origin string) (installation.LocalState, error) {
	state, err := controller.LocalState(ctx, origin)
	if err != nil {
		return installation.LocalState{}, err
	}
	status, err := controller.installations.Status(ctx, origin)
	if err == nil {
		state.RelayActive = &status.RelayActive
	}
	return state, nil
}

func (controller *Controller) startReconnectLoop(installed desktopcontrol.Installation) {
	controller.mu.Lock()
	if controller.closed || controller.reconnectStarted {
		controller.mu.Unlock()
		return
	}
	controller.reconnectStarted = true
	controller.mu.Unlock()
	go func() {
		for controller.ctx.Err() == nil {
			status, err := controller.installations.Status(controller.ctx, controller.origin)
			controller.mu.Lock()
			if err == nil && status.RelayActive {
				controller.setupMayRunUnconfigured = false
			}
			expiredSetup := err == nil && !status.RelayActive && controller.setupMayRunUnconfigured && time.Now().After(controller.setupDeadline)
			if expiredSetup {
				controller.setupMayRunUnconfigured = false
			}
			shouldConnect := err == nil && (status.RelayActive || controller.setupMayRunUnconfigured)
			controller.mu.Unlock()
			if err == nil && !shouldConnect {
				controller.stopIdleRelay()
			}
			if shouldConnect {
				if status.RelayActive {
					if err := controller.resumeIdleRelay(controller.ctx); err != nil {
						if !wait(controller.ctx, controller.reconnectEvery) {
							return
						}
						continue
					}
				}
				_ = controller.reconcileGateway(controller.ctx, installed)
			}
			if !wait(controller.ctx, controller.reconnectEvery) {
				return
			}
		}
	}()
}

func (controller *Controller) resumeIdleRelay(ctx context.Context) error {
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed {
		controller.mu.Unlock()
		return ErrUnavailable
	}
	paused := controller.paused
	userPaused := controller.userPaused
	controller.mu.Unlock()
	if userPaused {
		return ErrUnavailable
	}
	if !paused {
		return nil
	}
	state, err := controller.probe.State(ctx)
	if err != nil || state != desktopexecutor.SessionLockUnlocked {
		return ErrSession
	}
	if _, err := controller.runtime.Prepare(ctx); err != nil {
		return err
	}
	if err := controller.installations.ReportReadiness(ctx, controller.origin, apicontract.DesktopControlReadinessReady); err != nil {
		return err
	}
	if err := controller.startExecutorAndMonitor(ctx); err != nil {
		return err
	}
	controller.mu.Lock()
	controller.paused = false
	controller.mu.Unlock()
	return nil
}

func (controller *Controller) stopIdleRelay() {
	select {
	case controller.prepareGate <- struct{}{}:
		defer func() { <-controller.prepareGate }()
	case <-controller.ctx.Done():
		return
	}
	controller.mu.Lock()
	if controller.closed || controller.setupMayRunUnconfigured {
		controller.mu.Unlock()
		return
	}
	controller.mu.Unlock()
	status, err := controller.installations.Status(controller.ctx, controller.origin)
	if err != nil || status.RelayActive {
		return
	}
	controller.lockStateMu.Lock()
	controller.mu.Lock()
	if controller.closed || controller.setupMayRunUnconfigured {
		controller.mu.Unlock()
		controller.lockStateMu.Unlock()
		return
	}
	connection := controller.connection
	executor := controller.executor
	stop := controller.monitorStop
	controller.monitorStop = nil
	controller.paused = true
	controller.mu.Unlock()
	if connection != nil {
		connection.Close()
	}
	if executor != nil {
		cleanupContext, cancel := context.WithTimeout(controller.ctx, 30*time.Second)
		_ = executor.SetSessionLockState(cleanupContext, desktopexecutor.SessionLockUnknown)
		cancel()
	}
	if stop != nil {
		stop()
	}
	controller.runtime.Stop()
	controller.lockStateMu.Unlock()
}

func (controller *Controller) reconcileGateway(ctx context.Context, installed desktopcontrol.Installation) error {
	select {
	case controller.connectGate <- struct{}{}:
		defer func() { <-controller.connectGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	controller.mu.Lock()
	if controller.closed || controller.executor == nil || !controller.executor.NativeReady() {
		controller.mu.Unlock()
		return ErrUnavailable
	}
	existing := controller.connection
	controller.mu.Unlock()
	if existing != nil {
		if !existing.Status().Connected {
			return ErrUnavailable
		}
		baseline := existing.Status().ReadinessSequence
		if err := existing.SetReadiness(controller.desiredReadiness()); err != nil {
			return err
		}
		return controller.waitForAcknowledgedReadiness(ctx, existing, baseline, controller.desiredReadiness(), false)
	}
	handler := func(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, emit func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		controller.lockStateMu.Lock()
		controller.mu.Lock()
		executor := controller.executor
		paused := controller.closed || controller.userPaused || controller.paused
		lockState := controller.currentLock
		controller.mu.Unlock()
		controller.lockStateMu.Unlock()
		if paused || lockState != desktopexecutor.SessionLockUnlocked || executor == nil {
			return gatewayFailure(frame, "desktop_executor_unavailable")
		}
		return executor.Handle(ctx, frame, emit)
	}
	connection, err := controller.newGateway(installed, controller.origin, handler, func() *agentgatewayruntime.DesktopControlDiagnostics {
		return nil
	})
	if err != nil {
		return err
	}
	if err := connection.ConnectWithLifetime(ctx, controller.ctx); err != nil {
		connection.Close()
		return err
	}
	baseline := connection.Status().ReadinessSequence
	controller.mu.Lock()
	if controller.closed || controller.connection != nil {
		controller.mu.Unlock()
		connection.Close()
		return ErrUnavailable
	}
	controller.connection = connection
	controller.mu.Unlock()
	controller.updateGatewayReadiness(connection)
	if err := controller.waitForAcknowledgedReadiness(ctx, connection, baseline, controller.desiredReadiness(), false); err != nil {
		go controller.watchGateway(connection)
		return err
	}
	go controller.watchGateway(connection)
	return nil
}

func (controller *Controller) desiredReadiness() string {
	controller.mu.Lock()
	state := controller.currentLock
	userPaused := controller.userPaused
	controller.mu.Unlock()
	if userPaused {
		return string(apicontract.DesktopControlReadinessPaused)
	}
	return readinessFor(state, controller.runtime.State().Ready && controller.nativeExecutorReady())
}

func (controller *Controller) nativeExecutorReady() bool {
	controller.mu.Lock()
	executor := controller.executor
	controller.mu.Unlock()
	return executor != nil && executor.NativeReady()
}

func (controller *Controller) waitForAcknowledgedReadiness(ctx context.Context, connection gateway, baseline uint64, expected string, resumeCandidate bool) error {
	status := connection.Status()
	if !status.Connected {
		return ErrUnavailable
	}
	requiredSequence := status.ReadinessSequence
	if requiredSequence <= baseline {
		if baseline == ^uint64(0) {
			return ErrUnavailable
		}
		requiredSequence = baseline + 1
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if resumeCandidate {
			controller.mu.Lock()
			lockState := controller.currentLock
			controller.mu.Unlock()
			if expected != string(apicontract.DesktopControlReadinessReady) || lockState != desktopexecutor.SessionLockUnlocked || !controller.runtime.State().Ready || !controller.nativeExecutorReady() {
				return lockReadinessError(readinessFor(lockState, controller.runtime.State().Ready && controller.nativeExecutorReady()))
			}
		} else {
			readiness := controller.desiredReadiness()
			if readiness != expected {
				return lockReadinessError(readiness)
			}
		}
		status := connection.Status()
		if !status.Connected {
			return ErrUnavailable
		}
		if status.Readiness == expected && status.ReadinessSequence >= requiredSequence && status.AcknowledgedHeartbeatSequence >= requiredSequence {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func lockReadinessError(readiness string) error {
	switch readiness {
	case "locked":
		return ErrLocked
	case "unknown":
		return ErrSession
	default:
		return ErrUnavailable
	}
}

func (controller *Controller) abortPrepare() {
	controller.lockStateMu.Lock()
	controller.mu.Lock()
	stop := controller.monitorStop
	controller.monitorStop = nil
	connection := controller.connection
	executor := controller.executor
	controller.paused = true
	controller.setupMayRunUnconfigured = false
	controller.mu.Unlock()
	if connection != nil {
		connection.Close()
	}
	if stop != nil {
		stop()
	}
	if executor != nil {
		_ = executor.SetSessionLockState(controller.ctx, desktopexecutor.SessionLockUnknown)
	}
	controller.lockStateMu.Unlock()
	controller.runtime.Stop()
}

func (controller *Controller) watchGateway(connection gateway) {
	_ = connection.Wait(controller.ctx)
	connection.Close()
	controller.lockStateMu.Lock()
	controller.mu.Lock()
	current := controller.connection == connection
	var executor *desktopexecutor.Executor
	if current {
		controller.currentLock = desktopexecutor.SessionLockUnknown
		executor = controller.executor
		if !controller.closed {
			controller.paused = true
		}
	}
	controller.mu.Unlock()
	if current && executor != nil {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = executor.SetSessionLockState(cleanupContext, desktopexecutor.SessionLockUnknown)
		cancel()
	}
	if current {
		controller.mu.Lock()
		if controller.connection == connection {
			controller.connection = nil
		}
		controller.mu.Unlock()
	}
	controller.lockStateMu.Unlock()
}

func (controller *Controller) Close(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case controller.closeGate <- struct{}{}:
		defer func() { <-controller.closeGate }()
	case <-ctx.Done():
		return false
	}
	controller.mu.Lock()
	if controller.closeComplete {
		clean := controller.closeSuccess
		controller.mu.Unlock()
		return clean
	}
	if !controller.closed {
		controller.closed = true
		controller.cancel()
		if controller.monitorStop != nil {
			controller.monitorStop()
		}
	}
	connection := controller.connection
	controller.connection = nil
	executor := controller.executor
	controller.mu.Unlock()
	if connection != nil {
		connection.Close()
	}
	clean := true
	if executor != nil {
		clean = executor.Close(ctx)
	}
	controller.runtime.Close()
	controller.mu.Lock()
	controller.closeComplete = clean
	controller.closeSuccess = clean
	if clean {
		controller.executor = nil
	}
	controller.mu.Unlock()
	return clean
}

func gatewayFailure(frame agentgatewayruntime.DesktopControlFrame, code string) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{
		Type: agentgatewayruntime.DesktopControlFrameFailure, Version: agentgatewayruntime.DesktopControlProtocolVersion,
		RequestID: frame.RequestID, ErrorCode: code, ErrorMessage: "Desktop Control is not ready on this Linux session.",
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
