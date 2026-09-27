package cuamcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

const pinnedCUAProtocolVersion = "2025-06-18"

func TestClientRunsBoundedMCPCallsInOneOwnedProcess(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	exitMarker := filepath.Join(t.TempDir(), "stdin-eof")
	helperDir := t.TempDir()
	clickStarted := filepath.Join(helperDir, "click-started")
	clickRelease := filepath.Join(helperDir, "click-release")
	client.environment = append(client.environment,
		"CUA_HELPER_EXIT_MARKER="+exitMarker,
		"CUA_HELPER_CLICK_STARTED="+clickStarted,
		"CUA_HELPER_CLICK_RELEASE="+clickRelease,
	)
	queuedCallReady := make(chan struct{})
	var queuedSignal sync.Once
	var armQueuedSignal atomic.Bool
	client.beforeCallLock = func(name string) {
		if name == "list_apps" && armQueuedSignal.Load() {
			queuedSignal.Do(func() { close(queuedCallReady) })
		}
	}
	catalog, err := client.Start(context.Background())
	if err != nil {
		t.Fatalf("start Cua client: %v", err)
	}
	t.Cleanup(client.Stop)
	if len(catalog.Available) != len(exposedTools) {
		t.Fatalf("available tools = %d, want the full reviewed catalog %d", len(catalog.Available), len(exposedTools))
	}
	var result json.RawMessage
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationInput, "not_reviewed", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("unreviewed tool error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationInput, "click", json.RawMessage(`{"x":1,"x":2}`)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("duplicate argument error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "clipboard_write", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("tool outside the Desktop Control operation error = %v", err)
	}
	activeCall := make(chan struct {
		result json.RawMessage
		err    error
	}, 1)
	go func() {
		result, callErr := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationInput, "click", json.RawMessage(`{"x":12,"y":34}`))
		activeCall <- struct {
			result json.RawMessage
			err    error
		}{result: result, err: callErr}
	}()
	waitForFile(t, clickStarted)
	armQueuedSignal.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		_, callErr := client.Call(ctx, agentgatewayruntime.DesktopControlOperationObserve, "list_apps", json.RawMessage(`{}`))
		queued <- callErr
	}()
	<-queuedCallReady
	cancel()
	if err := os.WriteFile(clickRelease, []byte("release"), 0o600); err != nil {
		t.Fatalf("release active Cua call: %v", err)
	}
	clickResult := <-activeCall
	if clickResult.err != nil || string(clickResult.result) != `{"content":[{"type":"text","text":"ok"}],"isError":false}` {
		t.Fatalf("active click while next call cancels = %s, %v", clickResult.result, clickResult.err)
	}
	if err := <-queued; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued canceled call error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationClipboard, "clipboard_read", json.RawMessage(`{}`)); !errors.Is(err, ErrToolFailed) {
		t.Fatalf("Cua error response = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationInput, "zoom", json.RawMessage(`{}`)); !errors.Is(err, ErrToolFailed) {
		t.Fatalf("Cua tool failure = %v", err)
	}
	result, err = client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "list_apps", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result), `"isError":false`) {
		t.Fatalf("later call after errors/cancellation = %s, %v", result, err)
	}
	health, err := client.HealthReport(context.Background())
	if err != nil || !health.ReadyFor("0.29.1") {
		t.Fatalf("Linux setup health report: %v", err)
	}
	if _, err := client.CheckPermissions(context.Background()); err != nil {
		t.Fatalf("read-only permission status: %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "check_permissions", json.RawMessage(`{"prompt":true}`)); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("remote permission prompt error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "get_screen_size", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("malformed tool result error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "list_apps", json.RawMessage(`{}`)); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("call after malformed result = %v", err)
	}
	client.Stop()
	if _, err := os.ReadFile(exitMarker); err != nil {
		t.Fatalf("Cua child did not observe graceful stdin EOF: %v", err)
	}
}

func TestClientDoesNotRetryUncertainToolOutcome(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	requestLog := filepath.Join(t.TempDir(), "tool-requests")
	dispatched := filepath.Join(t.TempDir(), "tool-dispatched")
	client.environment = append(client.environment,
		"CUA_HELPER_UNCERTAIN_OUTCOME=1",
		"CUA_HELPER_UNCERTAIN_REQUESTS="+requestLog,
		"CUA_HELPER_UNCERTAIN_DISPATCHED="+dispatched,
	)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start Cua client: %v", err)
	}
	t.Cleanup(client.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	callResult := make(chan error, 1)
	go func() {
		_, callErr := client.Call(ctx, agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`))
		callResult <- callErr
	}()
	waitForFile(t, dispatched)
	cancel()
	if err := <-callResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("uncertain tool result error = %v, want caller cancellation", err)
	}
	requests, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatalf("read fake Cua request log: %v", err)
	}
	if string(requests) != "get_desktop_state\n" {
		t.Fatalf("uncertain operation requests = %q, want one request", requests)
	}
}

func TestClientLifetimeOutlivesStartupContext(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	lifetimeContext, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	startupContext, cancelStartup := context.WithCancel(context.Background())
	if _, err := client.StartWithLifetime(startupContext, lifetimeContext); err != nil {
		t.Fatalf("start Cua client: %v", err)
	}
	t.Cleanup(client.Stop)
	cancelStartup()
	if !client.Alive() {
		t.Fatal("startup context cancellation stopped the Cua child")
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call() after startup context cancellation = %v", err)
	}
	cancelLifetime()
	waitForAlive(t, client, false)
}

func TestClientStopsOnUnconsumedOutputOverflow(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	gate := filepath.Join(t.TempDir(), "overflow-gate")
	client.environment = append(client.environment, "CUA_HELPER_OUTPUT_OVERFLOW=1", "CUA_HELPER_OUTPUT_OVERFLOW_GATE="+gate)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start Cua client: %v", err)
	}
	t.Cleanup(client.Stop)
	if err := os.WriteFile(gate, []byte("release"), 0o600); err != nil {
		t.Fatalf("release overflow child: %v", err)
	}
	waitForAlive(t, client, false)
}

func TestClientDetectsParentExitWhenDescendantKeepsOutputOpen(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	release := filepath.Join(t.TempDir(), "release-output-holder")
	client.environment = append(client.environment, "CUA_HELPER_HOLD_OUTPUT_CHILD=1", "CUA_HELPER_OUTPUT_RELEASE="+release)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start Cua client: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release"), 0o600)
		client.Stop()
	})
	waitForAliveWithin(t, client, false, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Call(ctx, agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`)); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("Call() after the Cua parent exited = %v", err)
	}
}

func TestClientStopsWhenOutputReaderFails(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"close_stdout", "oversized_frame"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal("resolve test executable")
			}
			client, err := New(executable)
			if err != nil {
				t.Fatalf("create Cua client: %v", err)
			}
			client.command = func(ctx context.Context, path string) *exec.Cmd {
				return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
			}
			release := filepath.Join(t.TempDir(), "release-helper")
			client.environment = append(client.environment, "CUA_HELPER_TERMINAL_OUTPUT="+mode, "CUA_HELPER_TERMINAL_OUTPUT_RELEASE="+release)
			if _, err := client.Start(context.Background()); err != nil {
				t.Fatalf("start Cua client: %v", err)
			}
			t.Cleanup(func() {
				_ = os.WriteFile(release, []byte("release"), 0o600)
				client.Stop()
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := client.Call(ctx, agentgatewayruntime.DesktopControlOperationObserve, "get_desktop_state", json.RawMessage(`{}`)); err == nil {
				t.Fatal("Call() succeeded after output reader failure")
			}
			waitForAlive(t, client, false)
		})
	}
}

func waitForAlive(t *testing.T, client *Client, want bool) {
	waitForAliveWithin(t, client, want, time.Second)
}

func waitForAliveWithin(t *testing.T, client *Client, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for client.Alive() != want {
		select {
		case <-deadline.C:
			t.Fatalf("client alive = %t, want %t", client.Alive(), want)
		case <-ticker.C:
		}
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		case <-ticker.C:
		}
	}
}

func TestValidateCatalogRequiresEveryReviewedTool(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(exposedTools))
	for name := range exposedTools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]listedTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, listedTool{Name: name})
	}
	encoded, err := json.Marshal(listToolsResult{Tools: tools})
	if err != nil {
		t.Fatal("encode complete tool catalog")
	}
	catalog, err := validateCatalog(encoded)
	if err != nil || len(catalog.Available) != len(exposedTools) {
		t.Fatalf("complete catalog result = %#v, %v", catalog, err)
	}
	tools = tools[1:]
	encoded, err = json.Marshal(listToolsResult{Tools: tools})
	if err != nil {
		t.Fatal("encode incomplete tool catalog")
	}
	if _, err := validateCatalog(encoded); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("catalog missing a reviewed tool error = %v", err)
	}
}

func TestCallRejectsMalformedMCPResults(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		mode string
		tool string
	}{
		{name: "missing content", mode: "empty_result", tool: "get_screen_size"},
		{name: "invalid content envelope", mode: "malformed_content", tool: "get_cursor_position"},
		{name: "invalid content block", mode: "malformed_block", tool: "get_desktop_state"},
		{name: "both result and error", mode: "dual_result_error", tool: "list_windows"},
		{name: "mismatched response id", mode: "mismatched_id", tool: "get_window_state"},
		{name: "response with null id", mode: "null_id_result", tool: "get_browser_state"},
		{name: "null-id notification", mode: "null_id_notification", tool: "get_screen_size"},
		{name: "response with method", mode: "response_with_method", tool: "get_cursor_position"},
		{name: "error missing numeric code", mode: "missing_error_code", tool: "get_accessibility_tree"},
		{name: "resource missing contents", mode: "resource_missing_contents", tool: "get_desktop_state"},
		{name: "structured content null", mode: "structured_content_null", tool: "get_window_state"},
		{name: "image data invalid base64", mode: "image_invalid_base64", tool: "get_browser_state"},
		{name: "resource blob invalid base64", mode: "resource_invalid_blob", tool: "get_screen_size"},
		{name: "isError null", mode: "is_error_null", tool: "get_cursor_position"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal("resolve test executable")
			}
			client, err := New(executable)
			if err != nil {
				t.Fatalf("create Cua client: %v", err)
			}
			client.command = func(ctx context.Context, path string) *exec.Cmd {
				return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
			}
			client.environment = append(client.environment, "CUA_HELPER_MALFORMED_MODE="+testCase.mode)
			if _, err := client.Start(context.Background()); err != nil {
				t.Fatalf("start Cua client: %v", err)
			}
			t.Cleanup(client.Stop)
			if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, testCase.tool, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("malformed MCP result error = %v", err)
			}
			if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "list_apps", json.RawMessage(`{}`)); !errors.Is(err, ErrNotStarted) {
				t.Fatalf("call after malformed MCP result = %v", err)
			}
		})
	}
}

func TestClientCancelsBlockedMCPRequestWrite(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("resolve test executable")
	}
	client, err := New(executable)
	if err != nil {
		t.Fatalf("create Cua client: %v", err)
	}
	client.command = func(ctx context.Context, path string) *exec.Cmd {
		return exec.CommandContext(ctx, path, "-test.run=^TestCUAHelperProcess$", "--", "mcp")
	}
	client.environment = append(client.environment, "CUA_HELPER_STALL_AFTER_LIST=1")
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start stalled Cua child: %v", err)
	}
	t.Cleanup(client.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	arguments := json.RawMessage(`{"payload":"` + strings.Repeat("x", 4*1024*1024) + `"}`)
	if _, err := client.Call(ctx, agentgatewayruntime.DesktopControlOperationInput, "click", arguments); !errors.Is(err, ErrRequestTimedOut) {
		t.Fatalf("blocked Cua request write error = %v", err)
	}
	if _, err := client.Call(context.Background(), agentgatewayruntime.DesktopControlOperationObserve, "list_apps", json.RawMessage(`{}`)); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("call after blocked write cancellation = %v", err)
	}
}

func TestNextLinePrioritizesCancellationAndStopOverBufferedOutput(t *testing.T) {
	t.Parallel()
	t.Run("canceled context", func(t *testing.T) {
		t.Parallel()
		output := make(chan outputLine, 1)
		output <- outputLine{data: []byte("response")}
		client := &Client{output: output}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := client.nextLine(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("nextLine() error = %v", err)
		}
		if len(output) != 1 {
			t.Fatal("canceled nextLine() consumed buffered output")
		}
	})
	t.Run("explicit stop", func(t *testing.T) {
		t.Parallel()
		output := make(chan outputLine, 1)
		output <- outputLine{data: []byte("response")}
		client := &Client{output: output}
		client.stopRequested.Store(true)
		if _, err := client.nextLine(context.Background()); !errors.Is(err, ErrProcessExited) {
			t.Fatalf("nextLine() error = %v", err)
		}
		if len(output) != 1 {
			t.Fatal("stopped nextLine() consumed buffered output")
		}
	})
	t.Run("natural process exit drains final response", func(t *testing.T) {
		t.Parallel()
		output := make(chan outputLine, 1)
		output <- outputLine{data: []byte("final response")}
		waited := make(chan error)
		close(waited)
		client := &Client{output: output, waited: waited}
		line, err := client.nextLine(context.Background())
		if err != nil || string(line) != "final response" {
			t.Fatalf("nextLine() = %q, %v", line, err)
		}
	})
}

func TestValidateCatalogRejectsDuplicateAndMalformedTools(t *testing.T) {
	t.Parallel()
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"tools":[{"name":"get_desktop_state"},{"name":"get_desktop_state"}]}`),
		json.RawMessage(`{"tools":[{"name":""}]}`),
		json.RawMessage(`{"tools":[],"tools":[]}`),
		json.RawMessage(`[]`),
	} {
		if _, err := validateCatalog(raw); !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("invalid catalog %s error = %v", raw, err)
		}
	}
}

func TestChildEnvironmentKeepsDesktopSessionAndDropsSecrets(t *testing.T) {
	t.Parallel()
	environment := childEnvironment([]string{
		"PATH=/usr/bin", "HOME=/home/test", "WAYLAND_DISPLAY=wayland-1",
		"XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus",
		"PERSONASTACK_MACHINE_CREDENTIAL=secret", "CUA_DRIVER_RS_TELEMETRY_ENABLED=1",
	})
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{
		"PATH=/usr/bin", "WAYLAND_DISPLAY=wayland-1", "XDG_RUNTIME_DIR=/run/user/1000",
		"CUA_DRIVER_RS_TELEMETRY_ENABLED=0", "CUA_DRIVER_RS_UPDATE_CHECK=false",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("child environment omitted %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "PERSONASTACK_MACHINE_CREDENTIAL") || strings.Contains(joined, "=secret") {
		t.Fatalf("child environment retained a secret: %s", joined)
	}
}

func TestCUAHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "mcp" {
		return
	}
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), maximumLineBytes)
	for input.Scan() {
		var request struct {
			ID     *uint64 `json:"id"`
			Method string  `json:"method"`
			Params struct {
				Name            string          `json:"name"`
				Arguments       json.RawMessage `json:"arguments"`
				ProtocolVersion string          `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.Unmarshal(input.Bytes(), &request); err != nil {
			os.Exit(3)
		}
		if request.ID == nil {
			continue
		}
		switch request.Method {
		case "initialize":
			if request.Params.ProtocolVersion != pinnedCUAProtocolVersion {
				os.Exit(20)
			}
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"protocolVersion\":%q,\"capabilities\":{},\"serverInfo\":{\"name\":\"fake-cua\",\"version\":\"test\"}}}\n", *request.ID, pinnedCUAProtocolVersion)
		case "tools/list":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[", *request.ID)
			names := make([]string, 0, len(exposedTools))
			for name := range exposedTools {
				names = append(names, name)
			}
			sort.Strings(names)
			for index, name := range names {
				if index > 0 {
					fmt.Print(",")
				}
				fmt.Printf("{\"name\":%q}", name)
			}
			fmt.Println(`,{"name":"health_report"}]}}`)
			if os.Getenv("CUA_HELPER_HOLD_OUTPUT_CHILD") == "1" {
				command := exec.Command(os.Args[0], "-test.run=^TestCUAOutputHolderProcess$")
				command.Stdout = os.Stdout
				command.Stderr = os.Stderr
				command.Env = append(os.Environ(), "CUA_HELPER_OUTPUT_RELEASE="+os.Getenv("CUA_HELPER_OUTPUT_RELEASE"))
				if err := command.Start(); err != nil {
					os.Exit(11)
				}
				return
			}
			if os.Getenv("CUA_HELPER_OUTPUT_OVERFLOW") == "1" {
				waitForHelperFile(os.Getenv("CUA_HELPER_OUTPUT_OVERFLOW_GATE"))
				for index := 0; index < 32; index++ {
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"index\":%d}}\n", index)
				}
				return
			}
			if os.Getenv("CUA_HELPER_STALL_AFTER_LIST") == "1" {
				for {
					time.Sleep(time.Hour)
				}
			}
		case "tools/call":
			if os.Getenv("CUA_HELPER_UNCERTAIN_OUTCOME") == "1" {
				requestLog, err := os.OpenFile(os.Getenv("CUA_HELPER_UNCERTAIN_REQUESTS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					os.Exit(12)
				}
				if _, err := fmt.Fprintln(requestLog, request.Params.Name); err != nil {
					_ = requestLog.Close()
					os.Exit(13)
				}
				if err := requestLog.Close(); err != nil {
					os.Exit(14)
				}
				if err := os.WriteFile(os.Getenv("CUA_HELPER_UNCERTAIN_DISPATCHED"), []byte("dispatched"), 0o600); err != nil {
					os.Exit(15)
				}
				continue
			}
			if os.Getenv("CUA_HELPER_HOLD_OUTPUT_CHILD") == "1" {
				return
			}
			if mode := os.Getenv("CUA_HELPER_TERMINAL_OUTPUT"); mode != "" {
				switch mode {
				case "close_stdout":
					_ = os.Stdout.Close()
				case "oversized_frame":
					_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", maximumLineBytes+1))
				}
				waitForHelperFile(os.Getenv("CUA_HELPER_TERMINAL_OUTPUT_RELEASE"))
				continue
			}
			mode := os.Getenv("CUA_HELPER_MALFORMED_MODE")
			if mode != "" {
				switch mode {
				case "empty_result":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{}}\n", *request.ID)
				case "malformed_content":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":\"invalid\"}}\n", *request.ID)
				case "malformed_block":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":123}]}}\n", *request.ID)
				case "dual_result_error":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[]},\"error\":{\"code\":-1,\"message\":\"bad\"}}\n", *request.ID)
				case "mismatched_id":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[]}}\n", *request.ID+1)
				case "null_id_result":
					fmt.Println(`{"jsonrpc":"2.0","id":null,"result":{"content":[]}}`)
				case "null_id_notification":
					fmt.Println(`{"jsonrpc":"2.0","id":null,"method":"notifications/progress","params":{}}`)
				case "response_with_method":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"notifications/progress\",\"result\":{\"content\":[]}}\n", *request.ID)
				case "missing_error_code":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"message\":\"bad\"}}\n", *request.ID)
				case "resource_missing_contents":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"resource\",\"resource\":{}}]}}\n", *request.ID)
				case "structured_content_null":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[],\"structuredContent\":null}}\n", *request.ID)
				case "image_invalid_base64":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"image\",\"data\":\"not-base64!\",\"mimeType\":\"image/png\"}]}}\n", *request.ID)
				case "resource_invalid_blob":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"resource\",\"resource\":{\"uri\":\"file:///test\",\"blob\":\"bad!\"}}]}}\n", *request.ID)
				case "is_error_null":
					fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[],\"isError\":null}}\n", *request.ID)
				}
				continue
			}
			if request.Params.Name == "click" {
				started := os.Getenv("CUA_HELPER_CLICK_STARTED")
				release := os.Getenv("CUA_HELPER_CLICK_RELEASE")
				if err := os.WriteFile(started, []byte("started"), 0o600); err != nil {
					os.Exit(9)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(release); err == nil {
						break
					}
					if time.Now().After(deadline) {
						os.Exit(10)
					}
					time.Sleep(time.Millisecond)
				}
			}
			if request.Params.Name == "get_screen_size" {
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":[]}\n", *request.ID)
				continue
			}
			if request.Params.Name == "check_permissions" {
				if string(request.Params.Arguments) != `{}` {
					os.Exit(7)
				}
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ready\"}],\"isError\":false}}\n", *request.ID)
				continue
			}
			if request.Params.Name == "health_report" {
				if string(request.Params.Arguments) != `{"include":["binary_version","platform_supported","session_active","ax_capability","screen_capture_capability"]}` {
					os.Exit(8)
				}
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ready\"}],\"structuredContent\":{\"schema_version\":\"1\",\"platform\":\"linux\",\"driver_version\":\"0.29.1\",\"overall\":\"ok\",\"checks\":[{\"name\":\"binary_version\",\"status\":\"pass\",\"message\":\"ready\"},{\"name\":\"platform_supported\",\"status\":\"pass\",\"message\":\"ready\"},{\"name\":\"session_active\",\"status\":\"pass\",\"message\":\"ready\"},{\"name\":\"ax_capability\",\"status\":\"pass\",\"message\":\"ready\"},{\"name\":\"screen_capture_capability\",\"status\":\"pass\",\"message\":\"ready\"}]},\"isError\":false}}\n", *request.ID)
				continue
			}
			if request.Params.Name == "clipboard_read" {
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-1,\"message\":\"private text\"}}\n", *request.ID)
				continue
			}
			if request.Params.Name == "zoom" {
				fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[],\"isError\":true}}\n", *request.ID)
				continue
			}
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}],\"isError\":false}}\n", *request.ID)
		default:
			os.Exit(4)
		}
	}
	if err := input.Err(); err != nil {
		os.Exit(5)
	}
	if marker := os.Getenv("CUA_HELPER_EXIT_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("stdin eof"), 0o600); err != nil {
			os.Exit(6)
		}
	}
	os.Exit(0)
}

func TestCUAOutputHolderProcess(t *testing.T) {
	release := os.Getenv("CUA_HELPER_OUTPUT_RELEASE")
	if release == "" {
		return
	}
	waitForHelperFile(release)
}

func waitForHelperFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}
