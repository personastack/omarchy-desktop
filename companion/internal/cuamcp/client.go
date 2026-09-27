package cuamcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

const (
	maximumFrameBytes = 32 * 1024 * 1024
	maximumLineBytes  = maximumFrameBytes
	startTimeout      = 15 * time.Second
	toolCallTimeout   = 60 * time.Second
	gracefulStopLimit = 2 * time.Second
	killWaitLimit     = 2 * time.Second
	protocolVersion   = "2025-06-18"
)

var (
	ErrUnavailable      = errors.New("Cua MCP unavailable")
	ErrAlreadyStarted   = errors.New("Cua MCP already started")
	ErrNotStarted       = errors.New("Cua MCP not started")
	ErrInvalidFrame     = errors.New("Cua MCP frame invalid")
	ErrProtocolMismatch = errors.New("Cua MCP protocol version mismatch")
	ErrInvalidCatalog   = errors.New("Cua MCP tool catalog invalid")
	ErrInvalidTool      = errors.New("Cua MCP tool not exposed")
	ErrToolFailed       = errors.New("Cua MCP tool failed")
	ErrProcessExited    = errors.New("Cua MCP process exited")
	ErrRequestTimedOut  = errors.New("Cua MCP request timed out")
	ErrResponseTooLarge = errors.New("Cua MCP response too large")
)

type Catalog struct {
	Available []string
}

type Client struct {
	executable     string
	environment    []string
	command        func(context.Context, string) *exec.Cmd
	beforeCallLock func(string)

	mu            sync.Mutex
	process       *exec.Cmd
	input         io.WriteCloser
	outputReader  *os.File
	output        <-chan outputLine
	outputStop    chan struct{}
	cancel        context.CancelFunc
	waited        <-chan error
	started       bool
	request       uint64
	catalog       map[string]struct{}
	active        atomic.Pointer[processState]
	stopRequested atomic.Bool
	alive         atomic.Bool
}

type processState struct {
	closed chan struct{}
	once   sync.Once
}

func (s *processState) shutdown() {
	s.once.Do(func() {
		close(s.closed)
	})
}

type outputLine struct {
	data []byte
	err  error
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    *int64  `json:"code"`
	Message *string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      clientInfo      `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion json.RawMessage `json:"protocolVersion"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type listToolsResult struct {
	Tools []listedTool `json:"tools"`
}

type listedTool struct {
	Name string `json:"name"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallResult struct {
	Content           []json.RawMessage `json:"content"`
	IsError           json.RawMessage   `json:"isError"`
	StructuredContent json.RawMessage   `json:"structuredContent"`
}

type contentBlock struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`
	Data     *string         `json:"data"`
	MIMEType *string         `json:"mimeType"`
	URI      *string         `json:"uri"`
	Name     *string         `json:"name"`
	Resource json.RawMessage `json:"resource"`
}

type embeddedResource struct {
	URI      string  `json:"uri"`
	MIMEType *string `json:"mimeType"`
	Text     *string `json:"text"`
	Blob     *string `json:"blob"`
}

var exposedTools = map[string]struct{}{
	"bring_to_front": {}, "browser_click": {}, "browser_dialog": {}, "browser_download": {},
	"browser_navigate": {}, "browser_pointer": {}, "browser_set_input_files": {}, "browser_type": {},
	"check_permissions": {}, "click": {}, "clipboard_read": {}, "clipboard_write": {},
	"double_click": {}, "drag": {}, "get_accessibility_tree": {}, "get_browser_state": {},
	"get_cursor_position": {}, "get_desktop_state": {}, "get_screen_size": {}, "get_window_state": {},
	"hotkey": {}, "invoke_menu": {}, "kill_app": {}, "launch_app": {}, "list_apps": {},
	"list_windows": {}, "move_cursor": {}, "press_key": {}, "right_click": {}, "scroll": {},
	"set_value": {}, "set_window_frame": {}, "type_text": {}, "zoom": {},
}

var setupTools = map[string]json.RawMessage{
	"check_permissions": json.RawMessage(`{}`),
	"health_report":     json.RawMessage(`{"include":["binary_version","platform_supported","session_active","ax_capability","screen_capture_capability"]}`),
}

var operationTools = map[agentgatewayruntime.DesktopControlOperation]map[string]struct{}{
	agentgatewayruntime.DesktopControlOperationObserve: {
		"get_desktop_state": {}, "get_accessibility_tree": {}, "get_window_state": {},
		"get_cursor_position": {}, "get_screen_size": {}, "list_apps": {}, "list_windows": {}, "get_browser_state": {},
	},
	agentgatewayruntime.DesktopControlOperationInput: {
		"move_cursor": {}, "click": {}, "double_click": {}, "right_click": {}, "drag": {}, "scroll": {},
		"type_text": {}, "press_key": {}, "hotkey": {}, "set_value": {}, "zoom": {},
	},
	agentgatewayruntime.DesktopControlOperationApplication: {
		"launch_app": {}, "bring_to_front": {}, "kill_app": {}, "list_apps": {},
	},
	agentgatewayruntime.DesktopControlOperationWindow: {
		"list_windows": {}, "get_window_state": {}, "set_window_frame": {}, "bring_to_front": {}, "invoke_menu": {},
	},
	agentgatewayruntime.DesktopControlOperationClipboard: {"clipboard_read": {}, "clipboard_write": {}},
	agentgatewayruntime.DesktopControlOperationBrowser: {
		"get_browser_state": {}, "browser_navigate": {}, "browser_click": {}, "browser_type": {},
		"browser_pointer": {}, "browser_dialog": {}, "browser_download": {}, "browser_set_input_files": {},
	},
}

func New(executable string) (*Client, error) {
	return NewWithEnvironment(executable, os.Environ())
}

func NewWithEnvironment(executable string, environment []string) (*Client, error) {
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, ErrUnavailable
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(resolved) {
		return nil, ErrUnavailable
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil, ErrUnavailable
	}
	return &Client{executable: resolved, environment: childEnvironment(environment)}, nil
}

func (c *Client) Start(ctx context.Context) (Catalog, error) {
	return c.StartWithLifetime(ctx, ctx)
}

// StartWithLifetime bounds startup requests by ctx while tying the child
// process lifetime to lifetime.
func (c *Client) StartWithLifetime(ctx context.Context, lifetime context.Context) (Catalog, error) {
	if ctx == nil || lifetime == nil {
		return Catalog{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, contextError(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return Catalog{}, ErrAlreadyStarted
	}
	if c.stopRequested.Load() {
		return Catalog{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, contextError(err)
	}
	if err := c.startProcess(lifetime); err != nil {
		c.stopLocked()
		return Catalog{}, err
	}
	if c.stopRequested.Load() {
		c.stopLocked()
		return Catalog{}, ErrUnavailable
	}
	deadlineContext, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	params, err := json.Marshal(initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    json.RawMessage(`{}`),
		ClientInfo:      clientInfo{Name: "personastack-omarchy-desktop", Version: "1"},
	})
	if err != nil {
		c.stopLocked()
		return Catalog{}, ErrUnavailable
	}
	initialized, err := c.requestLocked(deadlineContext, "initialize", params)
	if err != nil {
		c.stopLocked()
		return Catalog{}, err
	}
	var initialize initializeResult
	if !validObject(initialized) || json.Unmarshal(initialized, &initialize) != nil {
		c.stopLocked()
		return Catalog{}, ErrInvalidFrame
	}
	var negotiatedVersion string
	if len(initialize.ProtocolVersion) == 0 || json.Unmarshal(initialize.ProtocolVersion, &negotiatedVersion) != nil || !validProtocolVersion(negotiatedVersion) {
		c.stopLocked()
		return Catalog{}, ErrInvalidFrame
	}
	if negotiatedVersion != protocolVersion {
		c.stopLocked()
		return Catalog{}, fmt.Errorf("%w: expected %s, server negotiated %s", ErrProtocolMismatch, protocolVersion, negotiatedVersion)
	}
	if err := c.notifyLocked(deadlineContext, "notifications/initialized"); err != nil {
		c.stopLocked()
		return Catalog{}, err
	}
	listContext, listCancel := context.WithTimeout(ctx, startTimeout)
	defer listCancel()
	result, err := c.requestLocked(listContext, "tools/list", json.RawMessage(`{}`))
	if err != nil {
		c.stopLocked()
		return Catalog{}, err
	}
	catalog, err := validateCatalog(result)
	if err != nil {
		c.stopLocked()
		return Catalog{}, err
	}
	c.catalog = make(map[string]struct{}, len(catalog.Available))
	for _, name := range catalog.Available {
		c.catalog[name] = struct{}{}
	}
	var listed listToolsResult
	if json.Unmarshal(result, &listed) == nil {
		for _, tool := range listed.Tools {
			if _, setupOnly := setupTools[tool.Name]; setupOnly {
				c.catalog[tool.Name] = struct{}{}
			}
		}
	}
	return catalog, nil
}

func validProtocolVersion(value string) bool {
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Format("2006-01-02") == value
}

func (c *Client) Call(ctx context.Context, operation agentgatewayruntime.DesktopControlOperation, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if ctx == nil || !validObject(arguments) {
		return nil, ErrInvalidFrame
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if _, allowed := operationTools[operation][name]; !allowed {
		return nil, ErrInvalidTool
	}
	if c.beforeCallLock != nil {
		c.beforeCallLock(name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if !c.started {
		return nil, ErrNotStarted
	}
	if !c.alive.Load() {
		c.stopLocked()
		return nil, ErrProcessExited
	}
	if _, available := c.catalog[name]; !available {
		return nil, ErrInvalidTool
	}
	return c.callLocked(ctx, name, arguments)
}

// CheckPermissions performs the Cua driver's read-only setup check. It never prompts.
func (c *Client) CheckPermissions(ctx context.Context) (json.RawMessage, error) {
	return c.callSetupTool(ctx, "check_permissions")
}

// HealthReport reads Cua's Linux session, accessibility, and screen-capture readiness.
func (c *Client) HealthReport(ctx context.Context) (HealthReportSnapshot, error) {
	raw, err := c.callSetupTool(ctx, "health_report")
	if err != nil {
		return HealthReportSnapshot{}, err
	}
	return parseHealthReport(raw)
}

func (c *Client) callSetupTool(ctx context.Context, name string) (json.RawMessage, error) {
	arguments, allowed := setupTools[name]
	if ctx == nil || !allowed {
		return nil, ErrInvalidTool
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if !c.started {
		return nil, ErrNotStarted
	}
	if !c.alive.Load() {
		c.stopLocked()
		return nil, ErrProcessExited
	}
	if _, available := c.catalog[name]; !available {
		return nil, ErrInvalidTool
	}
	return c.callLocked(ctx, name, arguments)
}

func (c *Client) callLocked(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	callContext, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	params, err := json.Marshal(toolCallParams{Name: name, Arguments: arguments})
	if err != nil || len(params) > maximumFrameBytes {
		return nil, ErrInvalidFrame
	}
	result, err := c.requestLocked(callContext, "tools/call", params)
	if err != nil {
		if !errors.Is(err, ErrToolFailed) {
			processExited := !c.alive.Load() && !errors.Is(err, ErrRequestTimedOut) && !errors.Is(err, context.Canceled)
			c.stopLocked()
			if processExited {
				return nil, ErrProcessExited
			}
		}
		return nil, err
	}
	var callResult toolCallResult
	if !validToolCallResult(result) || json.Unmarshal(result, &callResult) != nil {
		c.stopLocked()
		return nil, ErrInvalidFrame
	}
	if len(callResult.IsError) > 0 {
		var isError bool
		if err := json.Unmarshal(callResult.IsError, &isError); err != nil {
			c.stopLocked()
			return nil, ErrInvalidFrame
		}
		if isError {
			return nil, ErrToolFailed
		}
	}
	return result, nil
}

func (c *Client) Stop() {
	c.stopRequested.Store(true)
	state := c.active.Load()
	if state != nil {
		state.shutdown()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// Alive reports whether the managed child process has exited.
func (c *Client) Alive() bool {
	return c.alive.Load()
}

func (c *Client) startProcess(parent context.Context) error {
	processContext, cancel := context.WithCancel(parent)
	state := &processState{closed: make(chan struct{})}
	cmd := exec.CommandContext(processContext, c.executable, "mcp")
	if c.command != nil {
		cmd = c.command(processContext, c.executable)
	}
	cmd.Env = c.environment
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("create Cua MCP input: %w", err)
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		cancel()
		_ = input.Close()
		return fmt.Errorf("create Cua MCP output: %w", err)
	}
	cmd.Stdout = stdoutWriter
	if err := cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return fmt.Errorf("start Cua MCP: %w", err)
	}
	_ = stdoutWriter.Close()
	lines := make(chan outputLine, 16)
	outputStop := make(chan struct{})
	waited := make(chan error, 1)
	readDone := make(chan struct{})
	c.alive.Store(true)
	go func() {
		readLines(stdout, lines, outputStop, cancel, func() { c.alive.Store(false) })
		close(readDone)
	}()
	go func() {
		waitErr := cmd.Wait()
		c.alive.Store(false)
		select {
		case <-readDone:
		case <-time.After(100 * time.Millisecond):
		}
		_ = stdout.Close()
		state.shutdown()
		waited <- waitErr
		close(waited)
	}()
	c.process = cmd
	c.input = input
	c.outputReader = stdout
	c.output = lines
	c.outputStop = outputStop
	c.cancel = cancel
	c.waited = waited
	c.started = true
	c.request = 0
	c.catalog = nil
	c.active.Store(state)
	if c.stopRequested.Load() {
		state.shutdown()
	}
	return nil
}

func (c *Client) requestLocked(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if !c.started || c.input == nil || c.output == nil {
		return nil, ErrNotStarted
	}
	if c.request == ^uint64(0) {
		return nil, ErrUnavailable
	}
	c.request++
	id := c.request
	request := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := c.writeLocked(ctx, request); err != nil {
		return nil, err
	}
	for {
		line, err := c.nextLine(ctx)
		if err != nil {
			return nil, err
		}
		if !wirejson.ValidUniqueJSON(line) {
			return nil, ErrInvalidFrame
		}
		var response rpcResponse
		if json.Unmarshal(line, &response) != nil || response.JSONRPC != "2.0" {
			return nil, ErrInvalidFrame
		}
		if len(response.ID) == 0 {
			if response.Method != "" && len(response.Result) == 0 && len(response.Error) == 0 {
				continue
			}
			return nil, ErrInvalidFrame
		}
		if bytes.Equal(response.ID, []byte("null")) || response.Method != "" {
			return nil, ErrInvalidFrame
		}
		var responseID uint64
		if json.Unmarshal(response.ID, &responseID) != nil {
			return nil, ErrInvalidFrame
		}
		if responseID != id {
			return nil, ErrInvalidFrame
		}
		hasResult := len(response.Result) > 0
		hasError := len(response.Error) > 0
		if hasResult == hasError {
			return nil, ErrInvalidFrame
		}
		if hasError {
			var rpcFailure rpcError
			if !validObject(response.Error) || json.Unmarshal(response.Error, &rpcFailure) != nil || rpcFailure.Code == nil || rpcFailure.Message == nil {
				return nil, ErrInvalidFrame
			}
			return nil, ErrToolFailed
		}
		if bytes.Equal(response.Result, []byte("null")) {
			return nil, ErrInvalidFrame
		}
		return response.Result, nil
	}
}

func (c *Client) notifyLocked(ctx context.Context, method string) error {
	if !c.started || c.input == nil {
		return ErrNotStarted
	}
	return c.writeLocked(ctx, rpcRequest{JSONRPC: "2.0", Method: method})
}

func (c *Client) writeLocked(ctx context.Context, request rpcRequest) error {
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded)+1 > maximumFrameBytes {
		return ErrInvalidFrame
	}
	if err := ctx.Err(); err != nil {
		return contextError(err)
	}
	encoded = append(encoded, '\n')
	input := c.input
	if input == nil {
		return ErrNotStarted
	}
	written := make(chan error, 1)
	go func() {
		count, writeErr := input.Write(encoded)
		if writeErr == nil && count != len(encoded) {
			writeErr = io.ErrShortWrite
		}
		written <- writeErr
	}()
	var stopped <-chan struct{}
	if state := c.active.Load(); state != nil {
		stopped = state.closed
	}
	select {
	case err := <-written:
		if err != nil {
			return fmt.Errorf("write Cua MCP request: %w", err)
		}
		return nil
	case <-ctx.Done():
		c.abortWriteLocked()
		waitForWrite(written)
		return contextError(ctx.Err())
	case <-stopped:
		c.abortWriteLocked()
		waitForWrite(written)
		return ErrProcessExited
	case <-c.waited:
		c.abortWriteLocked()
		waitForWrite(written)
		return ErrProcessExited
	}
}

func (c *Client) abortWriteLocked() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.process != nil && c.process.Process != nil {
		_ = c.process.Process.Kill()
	}
	c.stopLocked()
}

func waitForWrite(written <-chan error) {
	select {
	case <-written:
	case <-time.After(killWaitLimit):
	}
}

func (c *Client) nextLine(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if c.stopRequested.Load() {
		return nil, ErrProcessExited
	}
	state := c.active.Load()
	if state != nil {
		select {
		case <-state.closed:
			if c.stopRequested.Load() {
				return nil, ErrProcessExited
			}
			return c.drainOutput(ctx)
		default:
		}
	}
	select {
	case <-c.waited:
		return c.drainOutput(ctx)
	default:
	}
	select {
	case line, ok := <-c.output:
		if err := ctx.Err(); err != nil {
			return nil, contextError(err)
		}
		if c.stopRequested.Load() {
			return nil, ErrProcessExited
		}
		return outputLineResult(line, ok)
	default:
	}
	var stopped <-chan struct{}
	if state != nil {
		stopped = state.closed
	}
	select {
	case line, ok := <-c.output:
		if err := ctx.Err(); err != nil {
			return nil, contextError(err)
		}
		if c.stopRequested.Load() {
			return nil, ErrProcessExited
		}
		return outputLineResult(line, ok)
	case <-ctx.Done():
		return nil, contextError(ctx.Err())
	case <-c.waited:
		return c.drainOutput(ctx)
	case <-stopped:
		if c.stopRequested.Load() {
			return nil, ErrProcessExited
		}
		return c.drainOutput(ctx)
	}
}

func (c *Client) drainOutput(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	if c.stopRequested.Load() {
		return nil, ErrProcessExited
	}
	select {
	case line, ok := <-c.output:
		if err := ctx.Err(); err != nil {
			return nil, contextError(err)
		}
		if c.stopRequested.Load() {
			return nil, ErrProcessExited
		}
		return outputLineResult(line, ok)
	default:
		return nil, ErrProcessExited
	}
}

func outputLineResult(line outputLine, ok bool) ([]byte, error) {
	if !ok {
		return nil, ErrProcessExited
	}
	if line.err != nil {
		return nil, line.err
	}
	return line.data, nil
}

func (c *Client) stopLocked() {
	c.alive.Store(false)
	state := c.active.Swap(nil)
	if state != nil {
		state.shutdown()
	}
	if c.outputStop != nil {
		close(c.outputStop)
	}
	if c.input != nil {
		_ = c.input.Close()
	}
	if c.outputReader != nil {
		_ = c.outputReader.Close()
	}
	if c.waited != nil {
		select {
		case <-c.waited:
		case <-time.After(gracefulStopLimit):
			if c.cancel != nil {
				c.cancel()
			}
			if c.process != nil && c.process.Process != nil {
				_ = c.process.Process.Kill()
			}
			select {
			case <-c.waited:
			case <-time.After(killWaitLimit):
			}
		}
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.process != nil && c.process.Process != nil {
		_ = c.process.Process.Kill()
	}
	c.process = nil
	c.input = nil
	c.output = nil
	c.outputReader = nil
	c.outputStop = nil
	c.cancel = nil
	c.waited = nil
	c.started = false
	c.request = 0
	c.catalog = nil
}

func readLines(reader io.ReadCloser, lines chan<- outputLine, stop <-chan struct{}, cancel context.CancelFunc, fail func()) {
	defer func() {
		select {
		case <-stop:
		default:
			fail()
			cancel()
		}
		_ = reader.Close()
		close(lines)
	}()
	buffered := bufio.NewReader(reader)
	for {
		line, err := readBoundedLine(buffered)
		if len(line) > 0 {
			if !deliverOutput(lines, stop, outputLine{data: line}) {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = deliverOutput(lines, stop, outputLine{err: err})
			}
			return
		}
	}
}

func deliverOutput(lines chan<- outputLine, stop <-chan struct{}, line outputLine) bool {
	select {
	case lines <- line:
		return true
	case <-stop:
		return false
	default:
		return false
	}
}

func readBoundedLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maximumLineBytes {
			return nil, ErrResponseTooLarge
		}
		line = append(line, fragment...)
		if err == nil {
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return nil, ErrInvalidFrame
			}
			return nil, err
		}
	}
}

func validateCatalog(raw json.RawMessage) (Catalog, error) {
	var result listToolsResult
	if !validObject(raw) || json.Unmarshal(raw, &result) != nil || len(result.Tools) == 0 {
		return Catalog{}, ErrInvalidCatalog
	}
	seen := make(map[string]struct{}, len(result.Tools))
	for _, tool := range result.Tools {
		if tool.Name == "" {
			return Catalog{}, ErrInvalidCatalog
		}
		if _, exists := seen[tool.Name]; exists {
			return Catalog{}, ErrInvalidCatalog
		}
		seen[tool.Name] = struct{}{}
	}
	for required := range exposedTools {
		if _, exists := seen[required]; !exists {
			return Catalog{}, ErrInvalidCatalog
		}
	}
	available := make([]string, 0, len(exposedTools))
	for name := range exposedTools {
		if _, exists := seen[name]; exists {
			available = append(available, name)
		}
	}
	sort.Strings(available)
	return Catalog{Available: available}, nil
}

func validObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && wirejson.ValidUniqueJSON(trimmed)
}

func validToolCallResult(raw json.RawMessage) bool {
	var result toolCallResult
	if !validObject(raw) || json.Unmarshal(raw, &result) != nil || result.Content == nil {
		return false
	}
	if len(result.IsError) > 0 {
		var isError bool
		if bytes.Equal(result.IsError, []byte("null")) || json.Unmarshal(result.IsError, &isError) != nil {
			return false
		}
	}
	if len(result.StructuredContent) > 0 && !validObject(result.StructuredContent) {
		return false
	}
	for _, rawBlock := range result.Content {
		var block contentBlock
		if !validObject(rawBlock) || json.Unmarshal(rawBlock, &block) != nil {
			return false
		}
		switch block.Type {
		case "text":
			if block.Text == nil {
				return false
			}
		case "image", "audio":
			if block.Data == nil || *block.Data == "" || block.MIMEType == nil || *block.MIMEType == "" || !validBase64(*block.Data) {
				return false
			}
		case "resource_link":
			if block.Name == nil || *block.Name == "" || block.URI == nil || *block.URI == "" {
				return false
			}
		case "resource":
			if !validEmbeddedResource(block.Resource) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validBase64(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(value)
	return err == nil && len(decoded) > 0
}

func validEmbeddedResource(raw json.RawMessage) bool {
	var resource embeddedResource
	if !validObject(raw) || json.Unmarshal(raw, &resource) != nil || resource.URI == "" {
		return false
	}
	if resource.MIMEType != nil && *resource.MIMEType == "" {
		return false
	}
	return (resource.Text != nil) != (resource.Blob != nil) && (resource.Blob == nil || validBase64(*resource.Blob))
}

func childEnvironment(environment []string) []string {
	allowed := map[string]struct{}{
		"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {}, "TMPDIR": {},
		"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "DISPLAY": {}, "WAYLAND_DISPLAY": {},
		"XDG_RUNTIME_DIR": {}, "XDG_SESSION_TYPE": {}, "XDG_CURRENT_DESKTOP": {}, "HYPRLAND_INSTANCE_SIGNATURE": {},
		"XDG_SESSION_DESKTOP": {}, "XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {},
		"DBUS_SESSION_BUS_ADDRESS": {}, "AT_SPI_BUS_ADDRESS": {}, "XAUTHORITY": {},
		"GDK_BACKEND": {}, "QT_ACCESSIBILITY": {},
	}
	values := make(map[string]string, len(environment)+2)
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		if _, ok := allowed[key]; ok {
			values[key] = value
		}
	}
	values["CUA_DRIVER_RS_TELEMETRY_ENABLED"] = "0"
	values["CUA_DRIVER_RS_UPDATE_CHECK"] = "false"
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}

func contextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrRequestTimedOut
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return ErrUnavailable
}
