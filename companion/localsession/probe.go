package localsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	probeTimeout  = 8 * time.Second
	maxProbeBytes = 256 * 1024
)

var (
	ErrMissingHarness  = errors.New("selected local CLI is not installed")
	ErrOutdatedHarness = errors.New("selected local CLI lacks required plugin support")
	versionPattern     = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)
)

type HarnessInstallation struct {
	Executable  string
	Home        string
	Profile     string
	Shell       string
	Environment []string
}

type CommandRunner func(context.Context, string, []string, []string, int) ([]byte, error)

type Probe struct {
	run          CommandRunner
	environment  func() []string
	shell        func() (string, error)
	isExecutable func(string) bool
}

func NewProbe() Probe {
	return Probe{run: runCommand, environment: os.Environ, shell: loginShell, isExecutable: isExecutable}
}

// Inspect resolves the selected CLI in the user's login shell and verifies the
// finite plugin commands required by PersonaStack. It never logs command output.
func (p Probe) Inspect(ctx context.Context, harness string) (HarnessInstallation, error) {
	if !validHarness(harness) || p.run == nil || p.environment == nil || p.shell == nil || p.isExecutable == nil {
		return HarnessInstallation{}, ErrMissingHarness
	}
	shell, err := p.shell()
	if err != nil || !p.isExecutable(shell) {
		return HarnessInstallation{}, ErrMissingHarness
	}
	marker, err := newMarker()
	if err != nil {
		return HarnessInstallation{}, ErrMissingHarness
	}
	binary := "codex"
	if harness == HarnessClaudeCode {
		binary = "claude"
	}
	script := fmt.Sprintf("printf '\\n%%s\\0' %s; command -v %s; printf '\\0%%s\\0%%s\\0%%s\\0%%s\\0' \"$HOME\" \"${CODEX_HOME:-$HOME/.codex}\" \"${CLAUDE_CONFIG_DIR:-$HOME/.claude}\" \"$PATH\"", shellQuote(marker), binary)
	command := "exec /bin/sh -c " + shellQuote(script)
	output, err := p.run(ctx, shell, []string{"-l", "-c", command}, p.environment(), maxProbeBytes)
	if err != nil || len(output) > maxProbeBytes {
		return HarnessInstallation{}, fmt.Errorf("resolve %s executable: %w", harness, ErrMissingHarness)
	}
	fields, err := parseProbeEnvironment(output, marker)
	if err != nil || !p.isExecutable(fields[0]) {
		return HarnessInstallation{}, fmt.Errorf("resolve %s executable: %w", harness, ErrMissingHarness)
	}
	environment := replaceEnvironment(p.environment(), map[string]string{
		"HOME": fields[1], "CODEX_HOME": fields[2], "CLAUDE_CONFIG_DIR": fields[3], "PATH": fields[4],
	})
	if err := p.checkCapabilities(ctx, harness, fields[0], environment); err != nil {
		return HarnessInstallation{}, err
	}
	profile := fields[2]
	if harness == HarnessClaudeCode {
		profile = fields[3]
	}
	return HarnessInstallation{
		Executable: fields[0], Home: fields[1], Profile: profile, Shell: shell, Environment: environment,
	}, nil
}

func (p Probe) checkCapabilities(ctx context.Context, harness, executable string, environment []string) error {
	version, err := p.read(ctx, executable, environment, "--version")
	if err != nil {
		return fmt.Errorf("read %s version: %w", harness, ErrMissingHarness)
	}
	help, err := p.read(ctx, executable, environment, "--help")
	if err != nil {
		return fmt.Errorf("read %s capabilities: %w", harness, ErrOutdatedHarness)
	}
	plugin, err := p.read(ctx, executable, environment, "plugin", "--help")
	if err != nil {
		return fmt.Errorf("read %s plugin capabilities: %w", harness, ErrOutdatedHarness)
	}
	marketplace, err := p.read(ctx, executable, environment, "plugin", "marketplace", "--help")
	if err != nil {
		return fmt.Errorf("read %s marketplace capabilities: %w", harness, ErrOutdatedHarness)
	}
	if !validCapabilities(harness, version, help, plugin, marketplace) {
		return fmt.Errorf("validate %s plugin capabilities: %w", harness, ErrOutdatedHarness)
	}
	return nil
}

func (p Probe) read(ctx context.Context, executable string, environment []string, arguments ...string) (string, error) {
	output, err := p.run(ctx, executable, arguments, environment, maxProbeBytes)
	if err != nil || len(output) > maxProbeBytes || !utf8.Valid(output) {
		return "", ErrMissingHarness
	}
	return string(output), nil
}

func validCapabilities(harness, version, help, plugin, marketplace string) bool {
	minimum := [3]int{0, 154, 0}
	pluginAction := "add"
	if harness == HarnessClaudeCode {
		minimum = [3]int{2, 1, 152}
		pluginAction = "install"
	}
	match := versionPattern.FindString(version)
	parts := strings.Split(match, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]int
	for index, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		numbers[index] = number
	}
	if lessVersion(numbers, minimum) {
		return false
	}
	return strings.Contains(help, "plugin") && strings.Contains(plugin, pluginAction) &&
		strings.Contains(marketplace, "add") && strings.Contains(marketplace, "list")
}

func lessVersion(left, right [3]int) bool {
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

func parseProbeEnvironment(output []byte, marker string) ([]string, error) {
	if !utf8.Valid(output) {
		return nil, ErrMissingHarness
	}
	needle := []byte("\n" + marker + "\x00")
	index := bytes.Index(output, needle)
	if index < 0 {
		return nil, ErrMissingHarness
	}
	fields := strings.Split(string(output[index+len(needle):]), "\x00")
	if len(fields) != 6 || fields[5] != "" {
		return nil, ErrMissingHarness
	}
	fields = fields[:5]
	fields[0] = strings.Trim(fields[0], "\r\n")
	for _, field := range fields[:4] {
		if !filepath.IsAbs(field) || strings.ContainsAny(field, "\r\n") {
			return nil, ErrMissingHarness
		}
	}
	if strings.ContainsAny(fields[4], "\r\n") {
		return nil, ErrMissingHarness
	}
	return fields, nil
}

func replaceEnvironment(inherited []string, replacements map[string]string) []string {
	result := make([]string, 0, len(inherited)+len(replacements))
	seen := make(map[string]bool, len(replacements))
	for _, item := range inherited {
		key, _, found := strings.Cut(item, "=")
		if !found {
			result = append(result, item)
			continue
		}
		value, replace := replacements[key]
		if replace {
			result = append(result, key+"="+value)
			seen[key] = true
			continue
		}
		result = append(result, item)
	}
	for key, value := range replacements {
		if !seen[key] {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func newMarker() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("PERSONASTACK_PROBE_%x", raw), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func loginShell() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[2] != current.Uid || !filepath.IsAbs(fields[6]) {
			continue
		}
		return fields[6], nil
	}
	return "", ErrMissingHarness
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func runCommand(parent context.Context, executable string, arguments, environment []string, limit int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = environment
	command.Stdin = strings.NewReader("")
	command.Stderr = io.Discard
	output := &limitedBuffer{limit: limit}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > buffer.limit-buffer.Len() {
		return 0, errors.New("command output exceeds limit")
	}
	return buffer.Buffer.Write(data)
}
