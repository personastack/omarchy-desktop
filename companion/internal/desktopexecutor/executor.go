package desktopexecutor

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

const (
	leaseIdleDuration = 90 * time.Second
	leaseMaxDuration  = 30 * time.Minute
	leaseSweepPeriod  = 5 * time.Second
)

var ErrUnavailable = errors.New("desktop executor unavailable")

type ToolRunner interface {
	Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error)
}

type LocalOperations interface {
	Call(context.Context, agentgatewayruntime.DesktopControlOperation, json.RawMessage, time.Duration) (json.RawMessage, error)
	ActiveProcesses() int
	CloseAll(context.Context) bool
}

type owner struct {
	installation string
	workspace    string
	config       string
	persona      string
	run          string
	generation   int64
}

type configScope struct{ installation, workspace, config string }
type bindingScope struct {
	config  configScope
	persona string
}
type lease struct {
	owner         owner
	config        configScope
	configVersion int64
	token         string
	started       time.Time
	lastActivity  time.Time
}

type activeCommand struct {
	owner         owner
	config        configScope
	configVersion int64
	cancel        context.CancelFunc
	done          chan struct{}
}

// Executor dispatches the reviewed Cua and optional local operation families
// under one installation-wide control lease.
type Executor struct {
	runner ToolRunner
	local  LocalOperations
	now    func() time.Time

	mu              sync.Mutex
	current         *lease
	revokedConfigs  map[configScope]int64
	revokedBindings map[bindingScope]int64
	epoch           uint64
	revocations     int
	nextCommandID   uint64
	active          map[uint64]activeCommand
	unavailable     bool
}

func New(runner ToolRunner) (*Executor, error) {
	return NewWithLocalOperations(runner, nil)
}

func NewWithLocalOperations(runner ToolRunner, local LocalOperations) (*Executor, error) {
	if runner == nil {
		return nil, ErrUnavailable
	}
	executor := &Executor{
		runner:          runner,
		local:           local,
		now:             time.Now,
		revokedConfigs:  make(map[configScope]int64),
		revokedBindings: make(map[bindingScope]int64),
		active:          make(map[uint64]activeCommand),
	}
	if local != nil {
		go executor.leaseExpiryLoop()
	}
	return executor, nil
}

func (e *Executor) Handle(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, emit func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
	if ctx == nil || frame.Type != agentgatewayruntime.DesktopControlFrameCommand || frame.Target == nil || strings.TrimSpace(frame.RequestID) == "" {
		return failure(frame, "invalid_arguments", "The Desktop Control command is invalid.")
	}
	if err := agentgatewayruntime.ValidateDesktopControlFrame(frame); err != nil {
		return failure(frame, "invalid_arguments", "The Desktop Control command is invalid.")
	}
	if err := ctx.Err(); err != nil {
		return failure(frame, "desktop_command_failed", "The desktop command did not complete.")
	}
	if !e.deadlineValid(frame.DeadlineAt) {
		return failure(frame, "desktop_command_failed", "The desktop command deadline expired.")
	}
	target := *frame.Target
	commandOwner := owner{target.InstallationID, target.WorkspaceID, target.ConfigID, target.PersonaID, target.RunID, target.Generation}
	scope := configScope{target.InstallationID, target.WorkspaceID, target.ConfigID}
	if frame.Operation == agentgatewayruntime.DesktopControlOperationRevokeConfig {
		return e.revokeConfig(frame, scope, target.ConfigVersion)
	}
	if frame.Operation == agentgatewayruntime.DesktopControlOperationRevokeBinding {
		return e.revokeBinding(frame, scope, target.PersonaID, target.Generation, target.ConfigVersion)
	}
	if frame.Operation == agentgatewayruntime.DesktopControlOperationStatus {
		e.mu.Lock()
		configAuthorized := e.isConfigAuthorized(scope, target.ConfigVersion)
		bindingAuthorized := e.isBindingAuthorized(commandOwner)
		if !configAuthorized || !bindingAuthorized {
			e.mu.Unlock()
			if !configAuthorized {
				return failure(frame, "desktop_control_config_revoked", "This Desktop Control configuration is disabled or no longer authorized.")
			}
			return failure(frame, "desktop_control_binding_revoked", "This Desktop Control binding is no longer authorized.")
		}
		busy := e.leaseValid(e.current)
		e.mu.Unlock()
		if !emptyArguments(frame.Arguments) {
			return failure(frame, "invalid_arguments", "The Desktop Control status request is invalid.")
		}
		encoded, _ := json.Marshal(map[string]any{"available": false, "native_executor_ready": false, "busy": busy})
		return result(frame, encoded)
	}
	if frame.Operation == agentgatewayruntime.DesktopControlOperationAcquire {
		return e.acquire(frame, commandOwner, scope, target.ConfigVersion)
	}
	if frame.Operation == agentgatewayruntime.DesktopControlOperationRelease {
		return e.release(frame, commandOwner, scope, target.ConfigVersion)
	}
	isLocal := localOperation(frame.Operation)
	if !cuaOperation(frame.Operation) && !isLocal {
		return failure(frame, "desktop_executor_unavailable", "The Linux native file and process executor is not ready.")
	}
	var toolName string
	var arguments json.RawMessage
	var token string
	var ok bool
	if isLocal {
		arguments, token, ok = decodeLocalArguments(frame.Arguments)
	} else {
		toolName, arguments, token, ok = decodeCuaArguments(frame.Arguments)
		ok = ok && toolAllowed(frame.Operation, toolName)
	}
	if !ok {
		return failure(frame, "invalid_arguments", "The Desktop Control tool arguments are invalid.")
	}
	e.mu.Lock()
	if e.unavailable || isLocal && e.local == nil {
		e.mu.Unlock()
		return failure(frame, "desktop_executor_unavailable", "The Linux native file and process executor is not ready.")
	}
	active, epoch := e.authorize(commandOwner, scope, target.ConfigVersion, token)
	var commandID uint64
	var done chan struct{}
	var callCtx context.Context
	var cancel context.CancelFunc
	processTimeout := time.Duration(0)
	if active {
		deadline := frame.DeadlineAt
		leaseDeadline := e.current.started.Add(leaseMaxDuration)
		processTimeout = leaseDeadline.Sub(e.now())
		if leaseDeadline.Before(deadline) {
			deadline = leaseDeadline
		}
		callCtx, cancel = context.WithDeadline(ctx, deadline)
		commandID, done = e.registerActiveLocked(commandOwner, scope, target.ConfigVersion, cancel)
	}
	e.mu.Unlock()
	if !active {
		return failure(frame, "desktop_control_required", "Acquire the Desktop Control lease before sending input.")
	}
	defer e.finishActive(commandID, done)
	defer cancel()
	var response json.RawMessage
	var err error
	if isLocal {
		response, err = e.local.Call(callCtx, frame.Operation, arguments, processTimeout)
	} else {
		response, err = e.runner.Call(callCtx, frame.Operation, toolName, arguments)
	}
	if err != nil {
		var coded interface {
			DesktopControlCode() string
			DesktopControlMessage() string
		}
		if errors.As(err, &coded) {
			return failure(frame, coded.DesktopControlCode(), coded.DesktopControlMessage())
		}
		e.mu.Lock()
		stillAuthorized := e.authorizedAfterCall(commandOwner, scope, target.ConfigVersion, token, epoch)
		e.mu.Unlock()
		if !stillAuthorized {
			return failure(frame, "desktop_control_required", "Desktop Control authorization changed while the command was running.")
		}
		return failure(frame, "desktop_command_failed", "The desktop command failed.")
	}
	if !json.Valid(response) {
		return failure(frame, "desktop_command_failed", "The desktop command failed.")
	}
	if emit != nil && isProcessRead(frame.Operation) {
		if err := e.forwardProcessChunks(callCtx, frame, response, emit, commandOwner, scope, target.ConfigVersion, token, epoch); err != nil {
			return failure(frame, "desktop_command_failed", "The desktop command did not complete.")
		}
	}
	bounded, err := boundCuaResult(callCtx, response)
	if err != nil {
		if callCtx.Err() != nil {
			e.mu.Lock()
			stillAuthorized := e.authorizedAfterCall(commandOwner, scope, target.ConfigVersion, token, epoch)
			e.mu.Unlock()
			if !stillAuthorized {
				return failure(frame, "desktop_control_required", "Desktop Control authorization changed while the command was running.")
			}
			return failure(frame, "desktop_command_failed", "The desktop command exceeded its deadline.")
		}
		return failure(frame, "desktop_response_too_large", "The desktop returned a result larger than the supported response limit.")
	}
	e.mu.Lock()
	stillAuthorized := e.authorizedAfterCall(commandOwner, scope, target.ConfigVersion, token, epoch)
	if stillAuthorized && e.current != nil {
		e.current.lastActivity = e.now()
	}
	e.mu.Unlock()
	if !stillAuthorized {
		return failure(frame, "desktop_control_required", "Desktop Control authorization changed while the command was running.")
	}
	commandResult := result(frame, bounded)
	if _, err := agentgatewayruntime.MarshalDesktopControlFrame(commandResult); err != nil {
		return failure(frame, "desktop_response_too_large", "The desktop returned a result larger than the supported response limit.")
	}
	return commandResult
}

func (e *Executor) NativeReady() bool { return false }

func (e *Executor) forwardProcessChunks(ctx context.Context, command agentgatewayruntime.DesktopControlFrame, payload json.RawMessage,
	emit func(agentgatewayruntime.DesktopControlFrame) error, commandOwner owner, scope configScope, version int64,
	token string, epoch uint64) error {
	var output struct {
		ExecutionID string `json:"execution_id"`
		Chunks      []struct {
			Stream string `json:"stream"`
			Data   string `json:"data_base64"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(payload, &output); err != nil || output.ExecutionID == "" {
		return nil
	}
	for index, chunk := range output.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if chunk.Stream != "stdout" && chunk.Stream != "stderr" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil || len(data) == 0 {
			continue
		}
		e.mu.Lock()
		stillAuthorized := e.authorizedAfterCall(commandOwner, scope, version, token, epoch)
		e.mu.Unlock()
		if !stillAuthorized {
			return context.Canceled
		}
		frame := agentgatewayruntime.DesktopControlFrame{
			Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameResultChunk,
			RequestID: command.RequestID, StreamID: command.RequestID, Sequence: uint64(index + 1),
			StreamChannel: chunk.Stream, StreamData: data,
		}
		if err := emit(frame); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) acquire(frame agentgatewayruntime.DesktopControlFrame, commandOwner owner, scope configScope, version int64) agentgatewayruntime.DesktopControlFrame {
	if !emptyArguments(frame.Arguments) {
		return failure(frame, "invalid_arguments", "The Desktop Control lease request is invalid.")
	}
	e.mu.Lock()
	configAuthorized := e.isConfigAuthorized(scope, version)
	bindingAuthorized := e.isBindingAuthorized(commandOwner)
	if !configAuthorized || !bindingAuthorized {
		e.mu.Unlock()
		if !configAuthorized {
			return failure(frame, "desktop_control_config_revoked", "This Desktop Control configuration is disabled or no longer authorized.")
		}
		return failure(frame, "desktop_control_binding_revoked", "This Desktop Control binding is no longer authorized.")
	}
	if e.unavailable {
		e.mu.Unlock()
		return failure(frame, "desktop_executor_unavailable", "The desktop executor is unavailable because prior resource cleanup was not confirmed.")
	}
	if e.current != nil && !e.leaseValid(e.current) {
		previous := e.current.owner
		e.current = nil
		e.epoch++
		e.revocations++
		commands := e.cancelMatching(func(command activeCommand) bool { return command.owner == previous })
		e.mu.Unlock()
		if !e.completeRevocation(frame.DeadlineAt, commands, true) {
			return failure(frame, "desktop_control_revoke_incomplete", "The expired desktop command did not stop before a new lease was requested.")
		}
		return e.acquire(frame, commandOwner, scope, version)
	}
	if e.revocations > 0 {
		e.mu.Unlock()
		return failure(frame, "desktop_control_busy", "Another run holds the Desktop Control lease.")
	}
	if e.current != nil {
		if e.current.owner != commandOwner || e.current.config != scope || e.current.configVersion != version {
			e.mu.Unlock()
			return failure(frame, "desktop_control_busy", "Another run holds the Desktop Control lease.")
		}
		e.current.lastActivity = e.now()
		existing := e.current.token
		encoded, _ := json.Marshal(map[string]any{"control_token": existing, "expires_in_seconds": int64(leaseIdleDuration.Seconds())})
		e.mu.Unlock()
		return result(frame, encoded)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		e.mu.Unlock()
		return failure(frame, "desktop_executor_unavailable", "The Desktop Control lease could not be created.")
	}
	created := &lease{owner: commandOwner, config: scope, configVersion: version, token: hex.EncodeToString(raw[:]), started: e.now(), lastActivity: e.now()}
	e.current = created
	e.epoch++
	encoded, _ := json.Marshal(map[string]any{"control_token": created.token, "expires_in_seconds": int64(leaseIdleDuration.Seconds())})
	e.mu.Unlock()
	return result(frame, encoded)
}

func (e *Executor) release(frame agentgatewayruntime.DesktopControlFrame, commandOwner owner, scope configScope, version int64) agentgatewayruntime.DesktopControlFrame {
	var input struct {
		ControlToken string `json:"control_token"`
	}
	var fields map[string]json.RawMessage
	if !wirejson.ValidUniqueJSON(frame.Arguments) || json.Unmarshal(frame.Arguments, &fields) != nil || len(fields) != 1 || fields["control_token"] == nil ||
		json.Unmarshal(frame.Arguments, &input) != nil || strings.TrimSpace(input.ControlToken) == "" {
		return failure(frame, "invalid_arguments", "The Desktop Control lease token is required.")
	}
	e.mu.Lock()
	if !e.leaseMatches(commandOwner, scope, version, input.ControlToken) {
		e.mu.Unlock()
		return failure(frame, "desktop_control_required", "This run does not hold the Desktop Control lease.")
	}
	e.current = nil
	e.epoch++
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool { return command.owner == commandOwner })
	e.mu.Unlock()
	if !e.completeRevocation(frame.DeadlineAt, commands, true) {
		return failure(frame, "desktop_control_revoke_incomplete", "The active desktop command did not stop before release.")
	}
	return result(frame, json.RawMessage(`{"released":true}`))
}

func (e *Executor) revokeConfig(frame agentgatewayruntime.DesktopControlFrame, scope configScope, version int64) agentgatewayruntime.DesktopControlFrame {
	e.mu.Lock()
	if version > e.revokedConfigs[scope] {
		e.revokedConfigs[scope] = version
	}
	cleanupLocal := e.current != nil && e.current.config == scope && e.current.configVersion <= version
	if cleanupLocal {
		e.current = nil
		e.epoch++
	}
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool { return command.config == scope && command.configVersion <= version })
	e.mu.Unlock()
	if !e.completeRevocation(frame.DeadlineAt, commands, cleanupLocal) {
		return failure(frame, "desktop_control_revoke_incomplete", "An active desktop command did not stop before revocation.")
	}
	return result(frame, json.RawMessage(`{"revoked":true}`))
}

func (e *Executor) revokeBinding(frame agentgatewayruntime.DesktopControlFrame, scope configScope, persona string, generation, _ int64) agentgatewayruntime.DesktopControlFrame {
	e.mu.Lock()
	binding := bindingScope{scope, persona}
	if generation > e.revokedBindings[binding] {
		e.revokedBindings[binding] = generation
	}
	cleanupLocal := e.current != nil && e.current.config == scope && e.current.owner.persona == persona &&
		e.current.owner.generation <= generation
	if cleanupLocal {
		e.current = nil
		e.epoch++
	}
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool {
		return command.config == scope && command.owner.persona == persona && command.owner.generation <= generation
	})
	e.mu.Unlock()
	if !e.completeRevocation(frame.DeadlineAt, commands, cleanupLocal) {
		return failure(frame, "desktop_control_revoke_incomplete", "An active desktop command did not stop before revocation.")
	}
	return result(frame, json.RawMessage(`{"revoked":true}`))
}

func (e *Executor) authorize(commandOwner owner, scope configScope, version int64, token string) (bool, uint64) {
	if e.revocations > 0 || e.current == nil || !e.leaseMatches(commandOwner, scope, version, token) || !e.isConfigAuthorized(scope, version) || !e.isBindingAuthorized(commandOwner) {
		return false, e.epoch
	}
	e.current.lastActivity = e.now()
	return true, e.epoch
}

func (e *Executor) authorizedAfterCall(commandOwner owner, scope configScope, version int64, token string, epoch uint64) bool {
	return e.epoch == epoch && e.leaseMatches(commandOwner, scope, version, token) && e.isConfigAuthorized(scope, version) && e.isBindingAuthorized(commandOwner)
}

func (e *Executor) leaseMatches(commandOwner owner, scope configScope, version int64, token string) bool {
	if e.current == nil || e.current.owner != commandOwner || e.current.config != scope || e.current.configVersion != version || e.current.token != token {
		return false
	}
	if !e.leaseValid(e.current) {
		return false
	}
	return true
}

func (e *Executor) isConfigAuthorized(scope configScope, version int64) bool {
	if version < 0 {
		return false
	}
	revokedVersion, exists := e.revokedConfigs[scope]
	return !exists || version > revokedVersion
}
func (e *Executor) isBindingAuthorized(target owner) bool {
	return target.generation > e.revokedBindings[bindingScope{config: configScope{target.installation, target.workspace, target.config}, persona: target.persona}]
}

func (e *Executor) leaseValid(current *lease) bool {
	if current == nil {
		return false
	}
	now := e.now()
	return now.Sub(current.lastActivity) < leaseIdleDuration && now.Sub(current.started) < leaseMaxDuration
}

func (e *Executor) leaseExpiryLoop() {
	ticker := time.NewTicker(leaseSweepPeriod)
	defer ticker.Stop()
	for range ticker.C {
		e.expireLeaseIfNeeded()
	}
}

func (e *Executor) expireLeaseIfNeeded() {
	e.mu.Lock()
	if e.current == nil || e.revocations > 0 || e.unavailable {
		e.mu.Unlock()
		return
	}
	current := *e.current
	e.mu.Unlock()

	activeProcesses := e.local.ActiveProcesses() > 0
	e.mu.Lock()
	if e.current == nil || e.current.token != current.token || e.revocations > 0 {
		e.mu.Unlock()
		return
	}
	if activeProcesses && e.now().Sub(e.current.started) < leaseMaxDuration {
		e.current.lastActivity = e.now()
	}
	if e.leaseValid(e.current) {
		e.mu.Unlock()
		return
	}
	previous := e.current.owner
	e.current = nil
	e.epoch++
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool { return command.owner == previous })
	e.mu.Unlock()
	e.completeRevocation(time.Now().Add(leaseSweepPeriod), commands, true)
}

func (e *Executor) registerActiveLocked(commandOwner owner, scope configScope, version int64, cancel context.CancelFunc) (uint64, chan struct{}) {
	e.nextCommandID++
	done := make(chan struct{})
	e.active[e.nextCommandID] = activeCommand{owner: commandOwner, config: scope, configVersion: version, cancel: cancel, done: done}
	return e.nextCommandID, done
}

func (e *Executor) finishActive(id uint64, done chan struct{}) {
	e.mu.Lock()
	delete(e.active, id)
	close(done)
	e.mu.Unlock()
}

func (e *Executor) cancelMatching(matches func(activeCommand) bool) []<-chan struct{} {
	done := make([]<-chan struct{}, 0)
	for _, command := range e.active {
		if matches(command) {
			command.cancel()
			done = append(done, command.done)
		}
	}
	return done
}

func (e *Executor) completeRevocation(deadline time.Time, commands []<-chan struct{}, cleanupLocal bool) bool {
	if !waitForCommands(deadline, commands) {
		go func() {
			for _, done := range commands {
				<-done
			}
			cleaned := e.cleanupLocalResources(cleanupLocal, 5*time.Second)
			e.finishRevocation(cleaned)
		}()
		return false
	}
	cleaned := e.cleanupLocalResources(cleanupLocal, time.Until(deadline))
	e.finishRevocation(cleaned)
	return cleaned
}

func (e *Executor) cleanupLocalResources(cleanup bool, timeout time.Duration) bool {
	if !cleanup || e.local == nil {
		return true
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return e.local.CloseAll(ctx)
}

func (e *Executor) finishRevocation(cleaned bool) {
	e.mu.Lock()
	if !cleaned {
		e.unavailable = true
	}
	e.revocations--
	e.mu.Unlock()
}

func waitForCommands(deadline time.Time, commands []<-chan struct{}) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for _, done := range commands {
		select {
		case <-done:
		case <-timer.C:
			return false
		}
	}
	return true
}
func (e *Executor) deadlineValid(deadline time.Time) bool {
	return !deadline.IsZero() && deadline.After(e.now()) && deadline.Sub(e.now()) <= time.Minute
}

func decodeCuaArguments(raw json.RawMessage) (string, json.RawMessage, string, bool) {
	if !wirejson.ValidUniqueJSON(raw) {
		return "", nil, "", false
	}
	var args struct {
		ControlToken string          `json:"control_token"`
		Tool         string          `json:"tool"`
		Arguments    json.RawMessage `json:"arguments"`
	}
	var argsFields map[string]json.RawMessage
	if json.Unmarshal(raw, &argsFields) != nil || len(argsFields) != 3 || argsFields["control_token"] == nil || argsFields["tool"] == nil || argsFields["arguments"] == nil || json.Unmarshal(raw, &args) != nil || strings.TrimSpace(args.ControlToken) == "" || strings.TrimSpace(args.Tool) == "" || !isJSONObject(args.Arguments) {
		return "", nil, "", false
	}
	return args.Tool, args.Arguments, args.ControlToken, true
}

func decodeLocalArguments(raw json.RawMessage) (json.RawMessage, string, bool) {
	if !wirejson.ValidUniqueJSON(raw) {
		return nil, "", false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, "", false
	}
	var token string
	if field := fields["control_token"]; field == nil || json.Unmarshal(field, &token) != nil || strings.TrimSpace(token) == "" {
		return nil, "", false
	}
	delete(fields, "control_token")
	if len(fields) == 0 {
		return nil, "", false
	}
	arguments, err := json.Marshal(fields)
	return arguments, token, err == nil
}

func emptyArguments(raw json.RawMessage) bool {
	if !wirejson.ValidUniqueJSON(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	return json.Unmarshal(raw, &fields) == nil && len(fields) == 0
}

func isJSONObject(raw json.RawMessage) bool {
	if !json.Valid(raw) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func toolAllowed(operation agentgatewayruntime.DesktopControlOperation, name string) bool {
	_, allowed := operationTools[operation][name]
	return allowed
}

func cuaOperation(operation agentgatewayruntime.DesktopControlOperation) bool {
	return operationTools[operation] != nil
}

func localOperation(operation agentgatewayruntime.DesktopControlOperation) bool {
	switch operation {
	case agentgatewayruntime.DesktopControlOperationFile,
		agentgatewayruntime.DesktopControlOperationShellStart,
		agentgatewayruntime.DesktopControlOperationShellRead,
		agentgatewayruntime.DesktopControlOperationShellWrite,
		agentgatewayruntime.DesktopControlOperationShellStatus,
		agentgatewayruntime.DesktopControlOperationShellCancel:
		return true
	default:
		return false
	}
}

func isProcessRead(operation agentgatewayruntime.DesktopControlOperation) bool {
	return operation == agentgatewayruntime.DesktopControlOperationShellStart || operation == agentgatewayruntime.DesktopControlOperationShellRead
}

var operationTools = map[agentgatewayruntime.DesktopControlOperation]map[string]struct{}{
	agentgatewayruntime.DesktopControlOperationObserve:     set("get_desktop_state", "get_accessibility_tree", "get_window_state", "get_cursor_position", "get_screen_size", "list_apps", "list_windows", "get_browser_state"),
	agentgatewayruntime.DesktopControlOperationInput:       set("move_cursor", "click", "double_click", "right_click", "drag", "scroll", "type_text", "press_key", "hotkey", "set_value", "zoom"),
	agentgatewayruntime.DesktopControlOperationApplication: set("launch_app", "bring_to_front", "kill_app", "list_apps"),
	agentgatewayruntime.DesktopControlOperationWindow:      set("list_windows", "get_window_state", "set_window_frame", "bring_to_front", "invoke_menu"),
	agentgatewayruntime.DesktopControlOperationClipboard:   set("clipboard_read", "clipboard_write"),
	agentgatewayruntime.DesktopControlOperationBrowser:     set("get_browser_state", "browser_navigate", "browser_click", "browser_type", "browser_pointer", "browser_dialog", "browser_download", "browser_set_input_files"),
}

func set(names ...string) map[string]struct{} {
	values := make(map[string]struct{}, len(names))
	for _, name := range names {
		values[name] = struct{}{}
	}
	return values
}

func result(command agentgatewayruntime.DesktopControlFrame, payload json.RawMessage) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: command.RequestID, Result: payload}
}

func failure(command agentgatewayruntime.DesktopControlFrame, code, message string) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameFailure, RequestID: command.RequestID, ErrorCode: code, ErrorMessage: message}
}
