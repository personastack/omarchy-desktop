package localsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestProbeInspectsSupportedHarnesses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, harness, version, pluginAction string
		minimum                              [3]int
	}{
		{name: "codex", harness: HarnessCodex, version: "codex-cli 0.154.0", pluginAction: "add", minimum: [3]int{0, 154, 0}},
		{name: "claude", harness: HarnessClaudeCode, version: "2.1.152 (Claude Code)", pluginAction: "install", minimum: [3]int{2, 1, 152}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newProbeFixture(t, test.harness, test.version, test.pluginAction, "")
			installation, err := fixture.probe.Inspect(context.Background(), test.harness)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if installation.Executable != fixture.executable || installation.Shell != fixture.shell {
				t.Fatalf("installation paths = %#v", installation)
			}
			if installation.Home != fixture.home || installation.Profile != fixture.profile {
				t.Fatalf("installation home/profile = %#v", installation)
			}
			if !environmentHas(installation.Environment, "PATH", "/bin:/cli/bin") || !environmentHas(installation.Environment, "HOME", fixture.home) {
				t.Fatalf("resolved environment missing login-shell values: %#v", installation.Environment)
			}
			if !environmentHas(installation.Environment, "OPENAI_API_KEY", "fixture-user-value") {
				t.Fatal("probe did not preserve the inherited CLI environment")
			}
		})
	}
}

func TestProbeRejectsMissingOrOutdatedHarness(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, version, action string
		commandError          bool
		want                  error
	}{
		{name: "missing executable", commandError: true, want: ErrMissingHarness},
		{name: "old version", version: "0.153.9", action: "add", want: ErrOutdatedHarness},
		{name: "missing plugin command", version: "0.154.0", want: ErrOutdatedHarness},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newProbeFixture(t, HarnessCodex, test.version, test.action, "")
			if test.commandError {
				fixture.runHarness = func(_ []string) ([]byte, error) { return nil, errors.New("not installed") }
			}
			_, err := fixture.probe.Inspect(context.Background(), HarnessCodex)
			if !errors.Is(err, test.want) {
				t.Fatalf("Inspect() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestProbeRejectsMalformedLoginShellOutputAndOversizedCommands(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "bad marker", mode: "bad_marker"},
		{name: "relative executable", mode: "relative_executable"},
		{name: "invalid utf8", mode: "invalid_utf8"},
		{name: "oversized output", mode: "oversized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newProbeFixture(t, HarnessCodex, "0.154.0", "add", test.mode)
			_, err := fixture.probe.Inspect(context.Background(), HarnessCodex)
			if !errors.Is(err, ErrMissingHarness) {
				t.Fatalf("Inspect() error = %v, want %v", err, ErrMissingHarness)
			}
		})
	}
}

func TestValidCapabilitiesComparesSemanticVersions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, harness, version, plugin, marketplace string
		want                                        bool
	}{
		{name: "codex minimum", harness: HarnessCodex, version: "0.154.0", plugin: "plugin add", marketplace: "marketplace add list", want: true},
		{name: "codex below minimum", harness: HarnessCodex, version: "0.153.99", plugin: "plugin add", marketplace: "marketplace add list"},
		{name: "claude minimum", harness: HarnessClaudeCode, version: "2.1.152", plugin: "plugin install", marketplace: "marketplace add list", want: true},
		{name: "claude missing install", harness: HarnessClaudeCode, version: "2.1.153", plugin: "plugin", marketplace: "marketplace add list"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := validCapabilities(test.harness, test.version, "plugin", test.plugin, test.marketplace)
			if got != test.want {
				t.Fatalf("validCapabilities() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestParseProbeEnvironmentRequiresExactBoundedFields(t *testing.T) {
	t.Parallel()
	valid := []byte("login noise\nPERSONASTACK_PROBE_00000000000000000000000000000000\x00/usr/bin/codex\x00/home/eg\x00/home/eg/.codex\x00/home/eg/.claude\x00/bin:/usr/bin\x00")
	fields, err := parseProbeEnvironment(valid, "PERSONASTACK_PROBE_00000000000000000000000000000000")
	if err != nil || len(fields) != 5 {
		t.Fatalf("parseProbeEnvironment() = %v, %v", fields, err)
	}
	if _, err := parseProbeEnvironment(append(valid, []byte("extra\x00")...), "PERSONASTACK_PROBE_00000000000000000000000000000000"); err == nil {
		t.Fatal("accepted trailing field")
	}
}

func newProbeFixture(t *testing.T, harness, version, pluginAction, mode string) *probeFixture {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "codex")
	if harness == HarnessClaudeCode {
		executable = filepath.Join(root, "bin", "claude")
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	profile := filepath.Join(home, ".codex")
	if harness == HarnessClaudeCode {
		profile = filepath.Join(home, ".claude")
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &probeFixture{shell: "/bin/bash", executable: executable, home: home, profile: profile}
	fixture.runHarness = func(arguments []string) ([]byte, error) {
		switch strings.Join(arguments, " ") {
		case "--version":
			return []byte(version), nil
		case "--help":
			return []byte("plugin"), nil
		case "plugin --help":
			return []byte("plugin " + pluginAction), nil
		case "plugin marketplace --help":
			return []byte("marketplace add list"), nil
		default:
			return nil, errors.New("unplanned CLI command")
		}
	}
	run := func(ctx context.Context, path string, arguments, environment []string, maxBytes int) ([]byte, error) {
		if path == fixture.shell {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if mode == "oversized" {
				return bytesOf(maxBytes + 1), nil
			}
			joined := strings.Join(arguments, " ")
			marker := regexp.MustCompile(`PERSONASTACK_PROBE_[0-9a-f]{32}`).FindString(joined)
			if marker == "" {
				return nil, errors.New("marker missing")
			}
			if mode == "bad_marker" {
				return []byte("shell noise"), nil
			}
			resolved := fixture.executable
			if mode == "relative_executable" {
				resolved = "codex"
			}
			output := fmt.Sprintf("shell startup\n%s\x00%s\x00%s\x00%s\x00%s\x00/bin:/cli/bin\x00", marker, resolved, fixture.home, filepath.Join(fixture.home, ".codex"), filepath.Join(fixture.home, ".claude"))
			if mode == "invalid_utf8" {
				return append([]byte{0xff}, []byte(output)...), nil
			}
			return []byte(output), nil
		}
		if path != fixture.executable {
			return nil, errors.New("unexpected executable")
		}
		if !environmentHas(environment, "OPENAI_API_KEY", "fixture-user-value") {
			return nil, errors.New("inherited environment missing")
		}
		return fixture.runHarness(arguments)
	}
	fixture.probe = Probe{
		run: run, environment: func() []string { return []string{"HOME=/inherited", "PATH=/bin", "OPENAI_API_KEY=fixture-user-value"} },
		shell:        func() (string, error) { return fixture.shell, nil },
		isExecutable: func(path string) bool { return path == fixture.shell || path == fixture.executable },
	}
	return fixture
}

type probeFixture struct {
	probe      Probe
	shell      string
	executable string
	home       string
	profile    string
	runHarness func([]string) ([]byte, error)
}

func environmentHas(environment []string, key, want string) bool {
	for _, item := range environment {
		if name, value, ok := strings.Cut(item, "="); ok && name == key {
			return value == want
		}
	}
	return false
}

func bytesOf(size int) []byte { return []byte(strings.Repeat("x", size)) }
