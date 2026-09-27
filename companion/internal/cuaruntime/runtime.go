package cuaruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/personastack/omarchy-desktop/companion/internal/cuainstaller"
	"github.com/personastack/omarchy-desktop/companion/internal/cuamcp"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

var (
	ErrUnavailable     = errors.New("managed Cua runtime unavailable")
	ErrNotReady        = errors.New("managed Cua runtime did not pass Linux readiness")
	ErrUpgradeRequired = errors.New("PersonaStack Desktop upgrade required for the pinned Cua runtime")
)

type Installer interface {
	Install(context.Context) (string, error)
	Repair(context.Context) (string, error)
}

type Client interface {
	StartWithLifetime(context.Context, context.Context) (cuamcp.Catalog, error)
	CheckPermissions(context.Context) (json.RawMessage, error)
	HealthReport(context.Context) (cuamcp.HealthReportSnapshot, error)
	Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error)
	Alive() bool
	Stop()
}

type Factory func(context.Context, string) (Client, error)

type State struct {
	Ready           bool
	UpgradeRequired bool
	DriverVersion   string
	ToolCount       int
}

type Runtime struct {
	installer Installer
	factory   Factory

	mu              sync.Mutex
	prepareGate     chan struct{}
	client          Client
	preparingClient Client
	state           State
	setupCancel     context.CancelFunc
	runCancel       context.CancelFunc
	closed          bool
}

func New(installer Installer, factory Factory) (*Runtime, error) {
	if installer == nil || factory == nil {
		return nil, ErrUnavailable
	}
	return &Runtime{installer: installer, factory: factory, prepareGate: make(chan struct{}, 1)}, nil
}

func NewDefault() (*Runtime, error) {
	installer, err := cuainstaller.New()
	if err != nil {
		return nil, fmt.Errorf("create managed Cua installer: %w", err)
	}
	return New(installer, func(_ context.Context, executable string) (Client, error) {
		client, err := cuamcp.New(executable)
		if err != nil {
			return nil, fmt.Errorf("create Cua MCP client: %w", err)
		}
		return client, nil
	})
}

// Prepare installs the pinned driver if needed, starts one MCP child, and
// accepts it only when its versioned Linux health report passes.
func (r *Runtime) Prepare(ctx context.Context) (State, error) {
	return r.prepare(ctx, false)
}

// Repair restarts the owned MCP process and repairs only a verified
// PersonaStack-owned driver tree when its pinned files are damaged.
func (r *Runtime) Repair(ctx context.Context) (State, error) {
	return r.prepare(ctx, true)
}

func (r *Runtime) prepare(ctx context.Context, repair bool) (State, error) {
	if ctx == nil {
		return State{}, ErrUnavailable
	}
	select {
	case r.prepareGate <- struct{}{}:
		defer func() { <-r.prepareGate }()
	case <-ctx.Done():
		return State{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return State{}, ErrUnavailable
	}
	if !repair && r.state.Ready && r.client != nil && r.client.Alive() {
		state := r.state
		r.mu.Unlock()
		return state, nil
	}
	staleClient, staleCancel := r.detachLocked()
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		stopClient(staleClient, staleCancel)
		return State{}, err
	}
	setupCtx, setupCancel := context.WithCancel(ctx)
	processCtx, processCancel := context.WithCancel(context.Background())
	r.setupCancel = setupCancel
	r.mu.Unlock()
	stopClient(staleClient, staleCancel)
	accepted := false
	defer func() {
		setupCancel()
		if !accepted {
			processCancel()
		}
		r.mu.Lock()
		r.setupCancel = nil
		r.mu.Unlock()
	}()

	var executable string
	var err error
	if repair {
		executable, err = r.installer.Repair(setupCtx)
	} else {
		executable, err = r.installer.Install(setupCtx)
	}
	if err != nil {
		if repair {
			return State{}, fmt.Errorf("repair pinned Cua driver: %w", err)
		}
		return State{}, fmt.Errorf("install pinned Cua driver: %w", err)
	}
	if err := setupCtx.Err(); err != nil {
		return State{}, err
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return State{}, ErrUnavailable
	}
	client, err := r.factory(setupCtx, executable)
	if err != nil || client == nil {
		if err == nil {
			err = ErrUnavailable
		}
		return State{}, fmt.Errorf("create Cua client: %w", err)
	}
	clientAccepted := false
	defer func() {
		if !clientAccepted {
			client.Stop()
		}
		r.mu.Lock()
		if r.preparingClient == client {
			r.preparingClient = nil
		}
		r.mu.Unlock()
	}()
	if err := setupCtx.Err(); err != nil {
		return State{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return State{}, ErrUnavailable
	}
	r.preparingClient = client
	r.mu.Unlock()
	catalog, err := client.StartWithLifetime(setupCtx, processCtx)
	if err != nil {
		if errors.Is(err, cuamcp.ErrProtocolMismatch) {
			r.recordUpgradeRequired()
		}
		return State{}, fmt.Errorf("start Cua MCP process: %w", err)
	}
	if _, err := client.CheckPermissions(setupCtx); err != nil {
		return State{}, fmt.Errorf("check Cua Linux session permissions: %w", err)
	}
	report, err := client.HealthReport(setupCtx)
	if err != nil {
		return State{}, fmt.Errorf("read Cua Linux health report: %w", err)
	}
	if report.DriverVersion != cuainstaller.DriverVersion {
		r.recordUpgradeRequired()
		return State{}, ErrUpgradeRequired
	}
	if !report.ReadyFor(cuainstaller.DriverVersion) || !client.Alive() {
		return State{}, ErrNotReady
	}
	if err := setupCtx.Err(); err != nil {
		return State{}, err
	}
	r.mu.Lock()
	if r.closed || !client.Alive() {
		r.mu.Unlock()
		return State{}, ErrUnavailable
	}
	r.client = client
	r.preparingClient = nil
	r.state = State{Ready: true, DriverVersion: cuainstaller.DriverVersion, ToolCount: len(catalog.Available)}
	r.runCancel = processCancel
	r.setupCancel = nil
	accepted = true
	clientAccepted = true
	state := r.state
	r.mu.Unlock()
	return state, nil
}

func (r *Runtime) recordUpgradeRequired() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.state = State{UpgradeRequired: true}
	}
}

// Call routes the executor's finite Cua request to the ready managed child.
func (r *Runtime) Call(ctx context.Context, operation agentgatewayruntime.DesktopControlOperation, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		return nil, ErrUnavailable
	}
	r.mu.Lock()
	client := r.client
	if client == nil || !r.state.Ready || !client.Alive() {
		staleClient, staleCancel := r.detachLocked()
		r.mu.Unlock()
		stopClient(staleClient, staleCancel)
		return nil, ErrUnavailable
	}
	r.mu.Unlock()
	result, err := client.Call(ctx, operation, name, arguments)
	if !client.Alive() {
		r.mu.Lock()
		var staleClient Client
		var staleCancel context.CancelFunc
		if r.client == client {
			staleClient, staleCancel = r.detachLocked()
		}
		r.mu.Unlock()
		stopClient(staleClient, staleCancel)
	}
	return result, err
}

func (r *Runtime) State() State {
	r.mu.Lock()
	if r.client == nil || r.client.Alive() {
		state := r.state
		r.mu.Unlock()
		return state
	}
	client, cancel := r.detachLocked()
	r.mu.Unlock()
	stopClient(client, cancel)
	return State{}
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	setupCancel := r.setupCancel
	preparingClient := r.preparingClient
	r.preparingClient = nil
	client, runCancel := r.detachLocked()
	r.state = State{}
	r.mu.Unlock()
	if setupCancel != nil {
		setupCancel()
	}
	if preparingClient != nil && preparingClient != client {
		preparingClient.Stop()
	}
	stopClient(client, runCancel)
}

// Stop releases the managed child while keeping the runtime reusable for a
// later authorized setup. Close is reserved for process shutdown.
func (r *Runtime) Stop() {
	r.prepareGate <- struct{}{}
	defer func() { <-r.prepareGate }()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	client, runCancel := r.detachLocked()
	r.mu.Unlock()
	stopClient(client, runCancel)
}

func (r *Runtime) detachLocked() (Client, context.CancelFunc) {
	client, cancel := r.client, r.runCancel
	r.client = nil
	r.runCancel = nil
	r.state = State{UpgradeRequired: r.state.UpgradeRequired}
	return client, cancel
}

func stopClient(client Client, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	if client != nil {
		client.Stop()
	}
}
