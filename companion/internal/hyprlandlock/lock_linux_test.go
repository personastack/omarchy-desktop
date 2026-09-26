//go:build linux

package hyprlandlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/desktopexecutor"
)

type runnerFunc func(context.Context, string, ...string) ([]byte, error)

func (runner runnerFunc) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	return runner(ctx, executable, args...)
}

func TestProbeRequiresSessionEnvironmentAndParsesExactSnapshot(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	instanceDir := filepath.Join(runtimeDir, "hypr", "instance_123")
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"XDG_RUNTIME_DIR":             runtimeDir,
		"HYPRLAND_INSTANCE_SIGNATURE": "instance_123",
	}
	var calls int
	probe := NewProbe()
	probe.Executable = "/usr/bin/hyprctl"
	probe.LookupEnv = func(key string) (string, bool) { value, ok := environment[key]; return value, ok }
	probe.Runner = runnerFunc(func(_ context.Context, executable string, args ...string) ([]byte, error) {
		calls++
		if executable != "/usr/bin/hyprctl" || !reflect.DeepEqual(args, []string{"-j", "locked"}) {
			t.Fatalf("command = %q %q", executable, args)
		}
		return []byte(`{"locked":false}`), nil
	})
	state, err := probe.State(context.Background())
	if err != nil || state != desktopexecutor.SessionLockUnlocked || calls != 1 {
		t.Fatalf("State() = %q, %v, calls=%d", state, err, calls)
	}

	probe.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"locked":true}`), nil
	})
	state, err = probe.State(context.Background())
	if err != nil || state != desktopexecutor.SessionLockLocked {
		t.Fatalf("locked State() = %q, %v", state, err)
	}
	probe.CommandTimeout = 10 * time.Millisecond
	probe.Runner = runnerFunc(func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	state, err = probe.State(context.Background())
	if !errors.Is(err, ErrUnavailable) || state != desktopexecutor.SessionLockUnknown {
		t.Fatalf("timed-out State() = %q, %v", state, err)
	}
}

func TestProbeFailsClosedForInvalidEnvironmentAndOutput(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	instanceDir := filepath.Join(runtimeDir, "hypr", "valid")
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"XDG_RUNTIME_DIR": runtimeDir, "HYPRLAND_INSTANCE_SIGNATURE": "valid"}
	probe := NewProbe()
	probe.LookupEnv = func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	probe.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte(`{"locked":false}`), nil })
	for _, test := range []struct {
		name   string
		change func()
	}{
		{name: "missing runtime dir", change: func() { delete(values, "XDG_RUNTIME_DIR") }},
		{name: "relative runtime dir", change: func() { values["XDG_RUNTIME_DIR"] = "run/user/1000" }},
		{name: "invalid signature", change: func() { values["HYPRLAND_INSTANCE_SIGNATURE"] = "../other" }},
		{name: "missing instance dir", change: func() { values["HYPRLAND_INSTANCE_SIGNATURE"] = "absent" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			beforeRuntime, runtimePresent := values["XDG_RUNTIME_DIR"]
			beforeInstance, instancePresent := values["HYPRLAND_INSTANCE_SIGNATURE"]
			test.change()
			state, err := probe.State(context.Background())
			if !errors.Is(err, ErrUnavailable) || state != desktopexecutor.SessionLockUnknown {
				t.Fatalf("State() = %q, %v", state, err)
			}
			if runtimePresent {
				values["XDG_RUNTIME_DIR"] = beforeRuntime
			} else {
				delete(values, "XDG_RUNTIME_DIR")
			}
			if instancePresent {
				values["HYPRLAND_INSTANCE_SIGNATURE"] = beforeInstance
			} else {
				delete(values, "HYPRLAND_INSTANCE_SIGNATURE")
			}
		})
	}
	for _, output := range []string{
		`false`, `{"locked":"false"}`, `{"locked":false,"extra":true}`,
		`{"locked":false,"locked":true}`, `{"locked":false} {}`, `null`,
	} {
		probe.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte(output), nil })
		state, err := probe.State(context.Background())
		if !errors.Is(err, ErrUnavailable) || state != desktopexecutor.SessionLockUnknown {
			t.Fatalf("State(%s) = %q, %v", output, state, err)
		}
	}
}

type sinkRecorder struct {
	mu     sync.Mutex
	states []desktopexecutor.SessionLockState
}

func (sink *sinkRecorder) SetSessionLockState(_ context.Context, state desktopexecutor.SessionLockState) bool {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.states = append(sink.states, state)
	return true
}

func (sink *sinkRecorder) snapshot() []desktopexecutor.SessionLockState {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]desktopexecutor.SessionLockState(nil), sink.states...)
}

func TestMonitorReportsUnknownOnProbeFailureAndStopsOnContext(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runtimeDir, "hypr", "active"), 0o700); err != nil {
		t.Fatal(err)
	}
	probe := NewProbe()
	probe.LookupEnv = func(key string) (string, bool) {
		if key == "XDG_RUNTIME_DIR" {
			return runtimeDir, true
		}
		return "active", true
	}
	probe.Runner = runnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("compositor unavailable")
	})
	sink := &sinkRecorder{}
	monitor := &Monitor{Probe: probe, PollInterval: 500 * time.Millisecond, Sink: sink}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- monitor.Run(ctx) }()
	deadline := time.After(time.Second)
	for len(sink.snapshot()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("monitor did not report initial state")
		case <-time.After(time.Millisecond):
		}
	}
	states := sink.snapshot()
	if states[0] != desktopexecutor.SessionLockUnknown {
		t.Fatalf("initial state = %q", states[0])
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
}
