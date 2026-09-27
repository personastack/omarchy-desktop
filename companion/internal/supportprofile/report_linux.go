//go:build linux

package supportprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/cuainstaller"
)

const (
	maxCommandOutput = 32 * 1024
	probeTimeout     = 2 * time.Second
	hyprctlPath      = "/usr/bin/hyprctl"
	busctlPath       = "/usr/bin/busctl"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Report struct {
	SchemaVersion int       `json:"schema_version"`
	CollectedAt   time.Time `json:"collected_at"`
	Distribution  string    `json:"distribution,omitempty"`
	Version       string    `json:"distribution_version,omitempty"`
	Architecture  string    `json:"architecture"`
	Session       Session   `json:"session"`
	Hyprland      Component `json:"hyprland"`
	CuaDriver     Component `json:"cua_driver"`
	SecretService Component `json:"secret_service"`
}

type Session struct {
	Type                  string `json:"type,omitempty"`
	WaylandDisplayPresent bool   `json:"wayland_display_present"`
	X11DisplayPresent     bool   `json:"x11_display_present"`
	HyprlandInstance      bool   `json:"hyprland_instance_present"`
}

type Component struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
}

type Dependencies struct {
	Run        CommandRunner
	ReadFile   func(string) ([]byte, error)
	LookupEnv  func(string) string
	DriverPath func(context.Context) (string, error)
	Now        func() time.Time
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	command := exec.CommandContext(probeContext, name, args...)
	var output boundedOutput
	output.limit = maxCommandOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, err
	}
	return append([]byte(nil), output.Bytes()...), nil
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > output.limit {
		return 0, errors.New("support probe output exceeded its limit")
	}
	return output.Buffer.Write(data)
}

func Collect(ctx context.Context) (Report, error) {
	return CollectWith(ctx, Dependencies{
		Run: commandRunner{}, ReadFile: readOSReleaseFile, LookupEnv: os.Getenv,
		DriverPath: verifiedCuaDriverPath, Now: time.Now,
	})
}

func verifiedCuaDriverPath(ctx context.Context) (string, error) {
	installer, err := cuainstaller.New()
	if err != nil {
		return "", err
	}
	return installer.VerifiedExecutable(ctx)
}

func CollectWith(ctx context.Context, dependencies Dependencies) (Report, error) {
	if ctx == nil || dependencies.Run == nil || dependencies.ReadFile == nil || dependencies.LookupEnv == nil || dependencies.DriverPath == nil || dependencies.Now == nil {
		return Report{}, errors.New("support profile collector is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: 1,
		CollectedAt:   dependencies.Now().UTC(),
		Architecture:  runtime.GOARCH,
		Session: Session{
			Type:                  safeValue(dependencies.LookupEnv("XDG_SESSION_TYPE")),
			WaylandDisplayPresent: dependencies.LookupEnv("WAYLAND_DISPLAY") != "",
			X11DisplayPresent:     dependencies.LookupEnv("DISPLAY") != "",
			HyprlandInstance:      dependencies.LookupEnv("HYPRLAND_INSTANCE_SIGNATURE") != "",
		},
	}
	readOSRelease(&report, dependencies.ReadFile)
	report.Hyprland = probeVersion(ctx, dependencies.Run, hyprctlPath, "version")
	if driverPath, err := dependencies.DriverPath(ctx); err == nil {
		report.CuaDriver = probeVersion(ctx, dependencies.Run, driverPath, "--version")
	}
	report.SecretService.Available = commandAvailable(ctx, dependencies.Run, busctlPath, "--user", "--no-pager", "status", "org.freedesktop.secrets")
	return report, nil
}

func readOSReleaseFile(path string) ([]byte, error) {
	if path != "/etc/os-release" {
		return nil, errors.New("unexpected support profile file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxCommandOutput+1))
	if err != nil || len(contents) > maxCommandOutput {
		return nil, errors.New("support profile file exceeded its limit")
	}
	return contents, nil
}

func readOSRelease(report *Report, readFile func(string) ([]byte, error)) {
	contents, err := readFile("/etc/os-release")
	if err != nil || len(contents) > maxCommandOutput {
		return
	}
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"")
		switch key {
		case "ID":
			report.Distribution = safeValue(value)
		case "VERSION_ID":
			report.Version = safeValue(value)
		}
	}
}

func probeVersion(ctx context.Context, runner CommandRunner, name string, args ...string) Component {
	output, err := runner.Run(ctx, name, args...)
	if err != nil || len(output) > maxCommandOutput {
		return Component{}
	}
	version := safeValue(strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)[0])
	if version == "" {
		return Component{}
	}
	return Component{Available: true, Version: version}
}

func commandAvailable(ctx context.Context, runner CommandRunner, name string, args ...string) bool {
	_, err := runner.Run(ctx, name, args...)
	return err == nil
}

func safeValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}

func Encode(report Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
