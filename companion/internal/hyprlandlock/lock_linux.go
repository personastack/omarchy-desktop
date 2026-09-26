//go:build linux

package hyprlandlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/desktopexecutor"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
)

const (
	defaultCommandTimeout = 2 * time.Second
	defaultPollInterval   = time.Second
	maxCommandOutput      = 1024
)

var (
	ErrUnavailable  = errors.New("Hyprland lock state unavailable")
	instancePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, ErrUnavailable
	}
	return output.buffer.Bytes(), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	remaining := maxCommandOutput - buffer.buffer.Len()
	if remaining <= 0 || len(value) > remaining {
		buffer.exceeded = true
		return 0, ErrUnavailable
	}
	return buffer.buffer.Write(value)
}

// Probe queries one compositor snapshot. Environment and process access are
// injectable so malformed inputs and command failures stay deterministic.
type Probe struct {
	Executable     string
	CommandTimeout time.Duration
	LookupEnv      func(string) (string, bool)
	Runner         commandRunner
}

func NewProbe() *Probe {
	return &Probe{
		Executable:     "hyprctl",
		CommandTimeout: defaultCommandTimeout,
		LookupEnv:      os.LookupEnv,
		Runner:         osCommandRunner{},
	}
}

func (probe *Probe) State(ctx context.Context) (desktopexecutor.SessionLockState, error) {
	if ctx == nil || probe == nil {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return desktopexecutor.SessionLockUnknown, err
	}
	lookup := probe.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	runtimeDir, runtimeOK := lookup("XDG_RUNTIME_DIR")
	instance, instanceOK := lookup("HYPRLAND_INSTANCE_SIGNATURE")
	if !runtimeOK || !filepath.IsAbs(runtimeDir) || strings.TrimSpace(runtimeDir) != runtimeDir ||
		!instanceOK || !instancePattern.MatchString(instance) {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	instanceDir := filepath.Join(runtimeDir, "hypr", instance)
	info, err := os.Stat(instanceDir)
	if err != nil || !info.IsDir() {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	if probe.Runner == nil || strings.TrimSpace(probe.Executable) == "" {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	timeout := probe.CommandTimeout
	if timeout <= 0 || timeout > 10*time.Second {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := probe.Runner.Run(commandContext, probe.Executable, "-j", "locked")
	if err != nil || len(output) == 0 || len(output) > maxCommandOutput || !wirejson.ValidUniqueJSON(output) {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	var response struct {
		Locked *bool `json:"locked"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Locked == nil {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	if decoder.Decode(new(any)) != io.EOF {
		return desktopexecutor.SessionLockUnknown, ErrUnavailable
	}
	if *response.Locked {
		return desktopexecutor.SessionLockLocked, nil
	}
	return desktopexecutor.SessionLockUnlocked, nil
}

type stateSink interface {
	SetSessionLockState(context.Context, desktopexecutor.SessionLockState) bool
}

// Monitor reports an initial state and polls at a bounded, low frequency.
// Every read failure moves the executor to unknown immediately.
type Monitor struct {
	Probe        *Probe
	PollInterval time.Duration
	Sink         stateSink
}

func (monitor *Monitor) Run(ctx context.Context) error {
	if ctx == nil || monitor == nil || monitor.Probe == nil || monitor.Sink == nil {
		return ErrUnavailable
	}
	interval := monitor.PollInterval
	if interval == 0 {
		interval = defaultPollInterval
	}
	if interval < 500*time.Millisecond || interval > 30*time.Second {
		return ErrUnavailable
	}
	if err := monitor.readAndReport(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// A failed initial read is represented by the reported unknown state.
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := monitor.readAndReport(ctx); err != nil && errors.Is(err, context.Canceled) {
				return err
			}
		}
	}
}

func (monitor *Monitor) readAndReport(ctx context.Context) error {
	state, err := monitor.Probe.State(ctx)
	if err != nil {
		state = desktopexecutor.SessionLockUnknown
	}
	if !monitor.Sink.SetSessionLockState(ctx, state) {
		return ErrUnavailable
	}
	return err
}
