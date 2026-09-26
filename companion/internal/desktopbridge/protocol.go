package desktopbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/omarchy-desktop/companion/localsession"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const MaxRequestBytes = 13 * 1024 * 1024
const maxRequestID = 1<<53 - 1
const maxScopeLength = 512
const LifecycleScope = "desktop:lifecycle"

var (
	ErrInvalidRequest = errors.New("invalid desktop bridge request")
	ticketPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type Action string

const (
	ActionSync         Action = "sync"
	ActionState        Action = "state"
	ActionPrepare      Action = "prepare"
	ActionPause        Action = "pause"
	ActionResume       Action = "resume"
	ActionDisconnect   Action = "disconnect"
	ActionLocalSession Action = "local_session"
)

type Request struct {
	ID               uint64          `json:"id"`
	Version          string          `json:"version"`
	Action           Action          `json:"action"`
	Scope            string          `json:"scope"`
	EnrollmentTicket string          `json:"enrollment_ticket,omitempty"`
	LocalSession     json.RawMessage `json:"local_session,omitempty"`
}

type ErrorCode string

const (
	ErrorInvalidRequest      ErrorCode = "invalid_request"
	ErrorNotEnrolled         ErrorCode = "not_enrolled"
	ErrorKeyringUnavailable  ErrorCode = "keyring_unavailable"
	ErrorRejected            ErrorCode = "rejected"
	ErrorUnavailable         ErrorCode = "unavailable"
	ErrorInvalidBundle       ErrorCode = "invalid_bundle"
	ErrorStaleRequest        ErrorCode = "stale_request"
	ErrorMissingHarness      ErrorCode = "missing_harness"
	ErrorOutdatedHarness     ErrorCode = "outdated_harness"
	ErrorUnsafeFiles         ErrorCode = "unsafe_files"
	ErrorSessionLocked       ErrorCode = "session_locked"
	ErrorSessionStateUnknown ErrorCode = "session_state_unknown"
)

type Result struct {
	InstallationID      *string `json:"installation_id"`
	OperatingSystem     string  `json:"operating_system"`
	RuntimeAvailable    *bool   `json:"runtime_available,omitempty"`
	CuaReady            bool    `json:"cua_ready"`
	NativeExecutorReady bool    `json:"native_executor_ready"`
	GatewayConnected    bool    `json:"gateway_connected"`
	RelayActive         *bool   `json:"relay_active,omitempty"`
	RelayPaused         bool    `json:"relay_paused"`
	UserPaused          bool    `json:"user_paused"`
}

type Response struct {
	ID           uint64          `json:"id"`
	OK           bool            `json:"ok"`
	Error        ErrorCode       `json:"error,omitempty"`
	Result       *Result         `json:"result,omitempty"`
	LocalSession json.RawMessage `json:"local_session,omitempty"`
}

type Service interface {
	LocalState(context.Context, string) (installation.LocalState, error)
}

type Processor struct {
	mu             sync.Mutex
	service        Service
	localSessions  LocalSessionService
	controlRuntime ControlRuntime
	origin         string
	scope          string
	synced         bool
	generation     uint64
	nextCommandID  uint64
	commands       map[uint64]processorCommand
}

type processorCommand struct {
	cancel    context.CancelFunc
	lifecycle bool
}

type LocalSessionService interface {
	SynchronizeScope(string)
	Handle(context.Context, string, localsession.Command) (json.RawMessage, error)
}

type ControlRuntime interface {
	LocalState(context.Context, string) (installation.LocalState, error)
	LifecycleState(context.Context, string) (installation.LocalState, error)
	Prepare(context.Context, string, string) (installation.LocalState, error)
	Pause(context.Context) (installation.LocalState, error)
	Resume(context.Context) (installation.LocalState, error)
	Disconnect(context.Context) (installation.LocalState, error)
}

func New(service Service, origin string) (*Processor, error) {
	if service == nil {
		return nil, ErrInvalidRequest
	}
	if _, err := desktopcontrol.New(origin); err != nil {
		return nil, ErrInvalidRequest
	}
	return &Processor{service: service, origin: origin, commands: make(map[uint64]processorCommand)}, nil
}

func NewWithLocalSessions(service Service, localSessions LocalSessionService, origin string) (*Processor, error) {
	processor, err := New(service, origin)
	if err != nil || localSessions == nil {
		return nil, ErrInvalidRequest
	}
	processor.localSessions = localSessions
	return processor, nil
}

func NewWithControlRuntime(service Service, localSessions LocalSessionService, controlRuntime ControlRuntime, origin string) (*Processor, error) {
	processor, err := New(service, origin)
	if err != nil || controlRuntime == nil {
		return nil, ErrInvalidRequest
	}
	processor.localSessions = localSessions
	processor.controlRuntime = controlRuntime
	return processor, nil
}

func Parse(raw []byte) (Request, error) {
	if len(raw) == 0 || len(raw) > MaxRequestBytes || !utf8.Valid(raw) || !wirejson.ValidUniqueJSON(raw) {
		return Request{}, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return Request{}, ErrInvalidRequest
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Request{}, ErrInvalidRequest
	}
	var request Request
	if err := json.Unmarshal(raw, &request); err != nil || request.ID == 0 || request.ID > maxRequestID {
		return Request{}, ErrInvalidRequest
	}
	if request.Version != "1" || request.Scope != strings.TrimSpace(request.Scope) || len(request.Scope) > maxScopeLength {
		return Request{}, ErrInvalidRequest
	}
	if !hasField(fields, "id") || !hasField(fields, "version") || !hasField(fields, "action") || !hasField(fields, "scope") {
		return Request{}, ErrInvalidRequest
	}
	switch request.Action {
	case ActionSync:
		if len(fields) != 4 || hasField(fields, "enrollment_ticket") || request.Scope == LifecycleScope {
			return Request{}, ErrInvalidRequest
		}
	case ActionState, ActionPause, ActionResume, ActionDisconnect:
		if len(fields) != 4 || hasField(fields, "enrollment_ticket") {
			return Request{}, ErrInvalidRequest
		}
		if (request.Action == ActionPause || request.Action == ActionResume || request.Action == ActionDisconnect) && request.Scope != LifecycleScope {
			return Request{}, ErrInvalidRequest
		}
	case ActionPrepare:
		if len(fields) != 5 || request.Scope == "" || request.Scope == LifecycleScope || !hasField(fields, "enrollment_ticket") || !ticketPattern.MatchString(request.EnrollmentTicket) {
			return Request{}, ErrInvalidRequest
		}
	case ActionLocalSession:
		if len(fields) != 5 || request.Scope == "" || request.Scope == LifecycleScope || !hasField(fields, "local_session") || hasField(fields, "enrollment_ticket") {
			return Request{}, ErrInvalidRequest
		}
		command, err := localsession.Parse(request.LocalSession)
		if err != nil || command.Scope != request.Scope {
			return Request{}, ErrInvalidRequest
		}
	default:
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func (p *Processor) Handle(ctx context.Context, request Request) Response {
	if ctx == nil {
		ctx = context.Background()
	}
	response := Response{ID: request.ID}
	if request.Version != "1" || len(request.Scope) > maxScopeLength || request.Scope != strings.TrimSpace(request.Scope) {
		response.Error = ErrorInvalidRequest
		return response
	}
	if request.Action == ActionSync {
		p.synchronize(request.Scope)
		response.OK = true
		return response
	}
	lifecycleAction := request.Scope == LifecycleScope &&
		(request.Action == ActionState || request.Action == ActionPause || request.Action == ActionResume || request.Action == ActionDisconnect)
	var commandCtx context.Context
	var commandID, generation uint64
	var ok bool
	if lifecycleAction {
		commandCtx, commandID, generation = p.beginLifecycleCommand(ctx)
		ok = true
	} else {
		commandCtx, commandID, generation, ok = p.beginCommand(ctx, request.Scope)
	}
	if !ok {
		response.Error = ErrorInvalidRequest
		return response
	}
	defer p.finishCommand(commandID)
	if request.Action == ActionLocalSession {
		if p.localSessions == nil {
			response.Error = ErrorUnavailable
			return response
		}
		command, err := localsession.Parse(request.LocalSession)
		if err != nil || command.Scope != request.Scope {
			response.Error = ErrorInvalidRequest
			return response
		}
		if commandCtx.Err() != nil {
			response.Error = ErrorStaleRequest
			return response
		}
		result, err := p.localSessions.Handle(commandCtx, p.origin, command)
		if !p.commandCurrent(request.Scope, generation) {
			response.Error = ErrorStaleRequest
			return response
		}
		if err != nil {
			response.Error = localSessionErrorCode(err)
			return response
		}
		response.OK = true
		response.LocalSession = result
		return response
	}
	if request.Action != ActionState && request.Action != ActionPrepare && request.Action != ActionPause && request.Action != ActionResume && request.Action != ActionDisconnect {
		response.Error = ErrorInvalidRequest
		return response
	}
	if request.Action == ActionPrepare && (request.Scope == "" || !ticketPattern.MatchString(request.EnrollmentTicket)) {
		response.Error = ErrorInvalidRequest
		return response
	}
	var state installation.LocalState
	var err error
	switch {
	case lifecycleAction && request.Action == ActionState && p.controlRuntime != nil:
		state, err = p.controlRuntime.LifecycleState(commandCtx, p.origin)
	case lifecycleAction && request.Action == ActionPause && p.controlRuntime != nil:
		state, err = p.controlRuntime.Pause(commandCtx)
	case lifecycleAction && request.Action == ActionResume && p.controlRuntime != nil:
		state, err = p.controlRuntime.Resume(commandCtx)
	case lifecycleAction && request.Action == ActionDisconnect && p.controlRuntime != nil:
		state, err = p.controlRuntime.Disconnect(commandCtx)
	case request.Action == ActionPause || request.Action == ActionResume || request.Action == ActionDisconnect:
		err = ErrInvalidRequest
	case request.Action == ActionPrepare && p.controlRuntime != nil:
		state, err = p.controlRuntime.Prepare(commandCtx, p.origin, request.EnrollmentTicket)
	case p.controlRuntime != nil:
		state, err = p.controlRuntime.LocalState(commandCtx, p.origin)
	default:
		state, err = p.service.LocalState(commandCtx, p.origin)
	}
	if !lifecycleAction && !p.commandCurrent(request.Scope, generation) {
		response.Error = ErrorStaleRequest
		return response
	}
	if err != nil {
		response.Error = errorCode(err)
		return response
	}
	response.OK = true
	result := &Result{
		InstallationID: state.InstallationID, CuaReady: state.CuaReady,
		NativeExecutorReady: state.NativeExecutorReady, GatewayConnected: state.GatewayConnected,
		RelayActive: state.RelayActive, RelayPaused: state.RelayPaused, UserPaused: state.UserPaused, OperatingSystem: "linux",
	}
	runtimeAvailable := p.controlRuntime != nil
	result.RuntimeAvailable = &runtimeAvailable
	response.Result = result
	return response
}

func (p *Processor) beginLifecycleCommand(parent context.Context) (context.Context, uint64, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextCommandID++
	ctx, cancel := context.WithCancel(parent)
	p.commands[p.nextCommandID] = processorCommand{cancel: cancel, lifecycle: true}
	return ctx, p.nextCommandID, p.generation
}

func (p *Processor) synchronize(scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.synced && p.scope == scope {
		return
	}
	if p.localSessions != nil {
		p.localSessions.SynchronizeScope(scope)
	}
	p.scope, p.synced = scope, true
	p.generation++
	for _, command := range p.commands {
		if !command.lifecycle {
			command.cancel()
		}
	}
}

func (p *Processor) beginCommand(parent context.Context, scope string) (context.Context, uint64, uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.synced || p.scope != scope {
		return nil, 0, 0, false
	}
	p.nextCommandID++
	ctx, cancel := context.WithCancel(parent)
	p.commands[p.nextCommandID] = processorCommand{cancel: cancel}
	return ctx, p.nextCommandID, p.generation, true
}

func (p *Processor) finishCommand(id uint64) {
	p.mu.Lock()
	command := p.commands[id]
	delete(p.commands, id)
	p.mu.Unlock()
	if command.cancel != nil {
		command.cancel()
	}
}

func (p *Processor) commandCurrent(scope string, generation uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.synced && p.scope == scope && p.generation == generation
}

func (p *Processor) CancelAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, command := range p.commands {
		command.cancel()
	}
}

func errorCode(err error) ErrorCode {
	var coded interface{ DesktopControlErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.DesktopControlErrorCode() {
		case "session_locked":
			return ErrorSessionLocked
		case "session_state_unknown":
			return ErrorSessionStateUnknown
		}
	}
	switch {
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, desktopcontrol.ErrInvalidRequest):
		return ErrorInvalidRequest
	case errors.Is(err, credentialstore.ErrCredentialMissing):
		return ErrorNotEnrolled
	case errors.Is(err, credentialstore.ErrUnavailable):
		return ErrorKeyringUnavailable
	case errors.Is(err, desktopcontrol.ErrRejected):
		return ErrorRejected
	default:
		return ErrorUnavailable
	}
}

func localSessionErrorCode(err error) ErrorCode {
	switch {
	case errors.Is(err, localsession.ErrInvalidRequest):
		return ErrorInvalidRequest
	case errors.Is(err, localsession.ErrInvalidBundle):
		return ErrorInvalidBundle
	case errors.Is(err, localsession.ErrStaleRequest):
		return ErrorStaleRequest
	case errors.Is(err, localsession.ErrMissingHarness):
		return ErrorMissingHarness
	case errors.Is(err, localsession.ErrOutdatedHarness):
		return ErrorOutdatedHarness
	case errors.Is(err, localsession.ErrUnsafeFiles):
		return ErrorUnsafeFiles
	default:
		return ErrorUnavailable
	}
}

func hasField(fields map[string]json.RawMessage, field string) bool {
	_, ok := fields[field]
	return ok
}
