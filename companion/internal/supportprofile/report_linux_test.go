//go:build linux

package supportprofile

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	results map[string][]byte
	fail    map[string]bool
	calls   [][]string
}

func (runner *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	runner.calls = append(runner.calls, call)
	key := strings.Join(call, " ")
	if runner.fail[key] {
		return nil, errors.New("probe failed")
	}
	return runner.results[key], nil
}

func TestCollectWithCapturesBoundedSupportFacts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 10, 11, 12, 0, time.FixedZone("PDT", -7*60*60))
	runner := &fakeRunner{results: map[string][]byte{
		"/usr/bin/hyprctl version":      []byte("Hyprland 0.56.2 (tag: v0.56.2)\nflags: example\n"),
		"/managed/cua-driver --version": []byte("cua-driver 0.30.1\n"),
	}, fail: map[string]bool{
		"/usr/bin/busctl --user --no-pager status org.freedesktop.secrets": true,
	}}
	variables := map[string]string{
		"XDG_SESSION_TYPE":                "wayland",
		"WAYLAND_DISPLAY":                 "wayland-1",
		"DISPLAY":                         ":0",
		"HYPRLAND_INSTANCE_SIGNATURE":     "private-instance-signature",
		"PERSONASTACK_INSTALLATION_TOKEN": "must-not-appear",
	}
	dependencies := Dependencies{
		Run: runner,
		ReadFile: func(path string) ([]byte, error) {
			if path != "/etc/os-release" {
				t.Fatalf("unexpected file read: %s", path)
			}
			return []byte("ID=omarchy\nVERSION_ID=4.0.2-1\nPRETTY_NAME=Omarchy\n"), nil
		},
		LookupEnv:  func(name string) string { return variables[name] },
		DriverPath: func(context.Context) (string, error) { return "/managed/cua-driver", nil },
		Now:        func() time.Time { return now },
	}

	report, err := CollectWith(context.Background(), dependencies)
	if err != nil {
		t.Fatalf("CollectWith() error = %v", err)
	}
	if report.SchemaVersion != 1 || report.CollectedAt.Format(time.RFC3339) != "2026-09-27T17:11:12Z" {
		t.Fatalf("report metadata = %#v", report)
	}
	if report.Distribution != "omarchy" || report.Version != "4.0.2-1" || report.Architecture == "" {
		t.Fatalf("distribution facts = %#v", report)
	}
	if !report.Session.WaylandDisplayPresent || !report.Session.X11DisplayPresent || !report.Session.HyprlandInstance || report.Session.Type != "wayland" {
		t.Fatalf("session facts = %#v", report.Session)
	}
	if !report.Hyprland.Available || report.Hyprland.Version != "Hyprland 0.56.2 (tag: v0.56.2)" {
		t.Fatalf("Hyprland facts = %#v", report.Hyprland)
	}
	if !report.CuaDriver.Available || report.CuaDriver.Version != "cua-driver 0.30.1" || report.SecretService.Available {
		t.Fatalf("component facts = %#v", report)
	}
	encoded, err := Encode(report)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if strings.Contains(string(encoded), "private-instance-signature") || strings.Contains(string(encoded), "must-not-appear") || strings.Contains(string(encoded), "wayland-1") {
		t.Fatalf("sensitive session value leaked into report: %s", encoded)
	}
	wantCalls := [][]string{{"/usr/bin/hyprctl", "version"}, {"/managed/cua-driver", "--version"}, {"/usr/bin/busctl", "--user", "--no-pager", "status", "org.freedesktop.secrets"}}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("probe calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestCollectWithMarksUnavailableComponentsWithoutFailing(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: map[string][]byte{}, fail: map[string]bool{
		"/usr/bin/hyprctl version":                                         true,
		"/managed/cua-driver --version":                                    true,
		"/usr/bin/busctl --user --no-pager status org.freedesktop.secrets": true,
	}}
	report, err := CollectWith(context.Background(), Dependencies{
		Run: runner, ReadFile: func(string) ([]byte, error) { return nil, errors.New("not present") },
		LookupEnv:  func(string) string { return "" },
		DriverPath: func(context.Context) (string, error) { return "/managed/cua-driver", nil },
		Now:        func() time.Time { return time.Unix(1, 0) },
	})
	if err != nil {
		t.Fatalf("CollectWith() error = %v", err)
	}
	if report.Hyprland.Available || report.CuaDriver.Available || report.SecretService.Available {
		t.Fatalf("failed probes were reported available: %#v", report)
	}
}

func TestCollectWithRejectsMissingDependenciesAndCanceledContext(t *testing.T) {
	t.Parallel()
	if _, err := CollectWith(context.Background(), Dependencies{}); err == nil {
		t.Fatal("CollectWith() accepted missing dependencies")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dependencies := Dependencies{
		Run:       &fakeRunner{results: map[string][]byte{}, fail: map[string]bool{}},
		ReadFile:  func(string) ([]byte, error) { return nil, nil },
		LookupEnv: func(string) string { return "" }, DriverPath: func(context.Context) (string, error) { return "/managed/cua-driver", nil }, Now: time.Now,
	}
	if _, err := CollectWith(ctx, dependencies); !errors.Is(err, context.Canceled) {
		t.Fatalf("CollectWith(canceled) error = %v", err)
	}
}

func TestSafeValueRejectsMultilineAndOversizedValues(t *testing.T) {
	t.Parallel()
	if got := safeValue("line one\nline two"); got != "" {
		t.Fatalf("safeValue(multiline) = %q", got)
	}
	if got := safeValue(strings.Repeat("x", 513)); got != "" {
		t.Fatalf("safeValue(oversized) length = %d", len(got))
	}
}

func TestProbeVersionRequiresVersionOutput(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{results: map[string][]byte{"tool --version": []byte("\n")}, fail: map[string]bool{}}
	if component := probeVersion(context.Background(), runner, "tool", "--version"); component.Available || component.Version != "" {
		t.Fatalf("empty version response = %#v", component)
	}
}
