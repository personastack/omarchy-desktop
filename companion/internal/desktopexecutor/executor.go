package desktopexecutor

import (
	"context"
	"crypto/rand"
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
)

var ErrUnavailable = errors.New("desktop executor unavailable")

type ToolRunner interface {
	Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error)
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

// Executor dispatches the reviewed Cua operation families and protects them
// with one installation-wide control lease. Native file and process operations
// remain unavailable until their Linux implementations are installed.
type Executor struct {
	runner ToolRunner
	now    func() time.Time

	mu              sync.Mutex
	current         *lease
	revokedConfigs  map[configScope]int64
	revokedBindings map[bindingScope]int64
	epoch           uint64
	revocations     int
	nextCommandID   uint64
	active          map[uint64]activeCommand
}

func New(runner ToolRunner) (*Executor, error) {
	if runner == nil {
		return nil, ErrUnavailable
	}
	return &Executor{
		runner:          runner,
		now:             time.Now,
		revokedConfigs:  make(map[configScope]int64),
		revokedBindings: make(map[bindingScope]int64),
		active:          make(map[uint64]activeCommand),
	}, nil
}

func (e *Executor) Handle(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, _ func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
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
	if !cuaOperation(frame.Operation) {
		return failure(frame, "desktop_executor_unavailable", "The Linux native file and process executor is not ready.")
	}
	toolName, arguments, token, ok := decodeCuaArguments(frame.Arguments)
	if !ok || !toolAllowed(frame.Operation, toolName) {
		return failure(frame, "invalid_arguments", "The Desktop Control tool arguments are invalid.")
	}
	e.mu.Lock()
	active, epoch := e.authorize(commandOwner, scope, target.ConfigVersion, token)
	var commandID uint64
	var done chan struct{}
	var callCtx context.Context
	var cancel context.CancelFunc
	if active {
		deadline := frame.DeadlineAt
		leaseDeadline := e.current.started.Add(leaseMaxDuration)
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
	response, err := e.runner.Call(callCtx, frame.Operation, toolName, arguments)
	if err != nil {
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
	if e.current != nil && !e.leaseValid(e.current) {
		previous := e.current.owner
		e.current = nil
		e.epoch++
		e.revocations++
		commands := e.cancelMatching(func(command activeCommand) bool { return command.owner == previous })
		e.mu.Unlock()
		if !e.completeRevocation(frame.DeadlineAt, commands) {
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
	if !e.completeRevocation(frame.DeadlineAt, commands) {
		return failure(frame, "desktop_control_revoke_incomplete", "The active desktop command did not stop before release.")
	}
	return result(frame, json.RawMessage(`{"released":true}`))
}

func (e *Executor) revokeConfig(frame agentgatewayruntime.DesktopControlFrame, scope configScope, version int64) agentgatewayruntime.DesktopControlFrame {
	e.mu.Lock()
	if version > e.revokedConfigs[scope] {
		e.revokedConfigs[scope] = version
	}
	if e.current != nil && e.current.config == scope && e.current.configVersion <= version {
		e.current = nil
		e.epoch++
	}
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool { return command.config == scope && command.configVersion <= version })
	e.mu.Unlock()
	if !e.completeRevocation(frame.DeadlineAt, commands) {
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
	if e.current != nil && e.current.config == scope && e.current.owner.persona == persona &&
		e.current.owner.generation <= generation {
		e.current = nil
		e.epoch++
	}
	e.revocations++
	commands := e.cancelMatching(func(command activeCommand) bool {
		return command.config == scope && command.owner.persona == persona && command.owner.generation <= generation
	})
	e.mu.Unlock()
	if !e.completeRevocation(frame.DeadlineAt, commands) {
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

func (e *Executor) completeRevocation(deadline time.Time, commands []<-chan struct{}) bool {
	if waitForCommands(deadline, commands) {
		e.finishRevocation()
		return true
	}
	go func() {
		for _, done := range commands {
			<-done
		}
		e.finishRevocation()
	}()
	return false
}

func (e *Executor) finishRevocation() { e.mu.Lock(); e.revocations--; e.mu.Unlock() }

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
