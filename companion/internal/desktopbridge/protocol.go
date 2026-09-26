package desktopbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const MaxRequestBytes = 4 * 1024
const maxRequestID = 1<<53 - 1
const maxScopeLength = 512

var (
	ErrInvalidRequest = errors.New("invalid desktop bridge request")
	ticketPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

type Action string

const (
	ActionSync    Action = "sync"
	ActionState   Action = "state"
	ActionPrepare Action = "prepare"
)

type Request struct {
	ID               uint64 `json:"id"`
	Version          string `json:"version"`
	Action           Action `json:"action"`
	Scope            string `json:"scope"`
	EnrollmentTicket string `json:"enrollment_ticket,omitempty"`
}

type ErrorCode string

const (
	ErrorInvalidRequest     ErrorCode = "invalid_request"
	ErrorNotEnrolled        ErrorCode = "not_enrolled"
	ErrorKeyringUnavailable ErrorCode = "keyring_unavailable"
	ErrorRejected           ErrorCode = "rejected"
	ErrorUnavailable        ErrorCode = "unavailable"
)

type Result struct {
	InstallationID      *string `json:"installation_id"`
	OperatingSystem     string  `json:"operating_system"`
	RuntimeAvailable    *bool   `json:"runtime_available,omitempty"`
	CuaReady            bool    `json:"cua_ready"`
	NativeExecutorReady bool    `json:"native_executor_ready"`
	GatewayConnected    bool    `json:"gateway_connected"`
	RelayPaused         bool    `json:"relay_paused"`
}

type Response struct {
	ID     uint64    `json:"id"`
	OK     bool      `json:"ok"`
	Error  ErrorCode `json:"error,omitempty"`
	Result *Result   `json:"result,omitempty"`
}

type Service interface {
	LocalState(context.Context, string) (installation.LocalState, error)
}

type Processor struct {
	service Service
	origin  string
	scope   string
	synced  bool
}

func New(service Service, origin string) (*Processor, error) {
	if service == nil {
		return nil, ErrInvalidRequest
	}
	if _, err := desktopcontrol.New(origin); err != nil {
		return nil, ErrInvalidRequest
	}
	return &Processor{service: service, origin: origin}, nil
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
	case ActionSync, ActionState:
		if len(fields) != 4 || hasField(fields, "enrollment_ticket") {
			return Request{}, ErrInvalidRequest
		}
	case ActionPrepare:
		if len(fields) != 5 || request.Scope == "" || !hasField(fields, "enrollment_ticket") || !ticketPattern.MatchString(request.EnrollmentTicket) {
			return Request{}, ErrInvalidRequest
		}
	default:
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func (p *Processor) Handle(ctx context.Context, request Request) Response {
	response := Response{ID: request.ID}
	if request.Version != "1" || len(request.Scope) > maxScopeLength || request.Scope != strings.TrimSpace(request.Scope) {
		response.Error = ErrorInvalidRequest
		return response
	}
	if request.Action == ActionSync {
		p.scope, p.synced = request.Scope, true
		response.OK = true
		return response
	}
	if !p.synced || p.scope != request.Scope {
		response.Error = ErrorInvalidRequest
		return response
	}
	if request.Action != ActionState && request.Action != ActionPrepare {
		response.Error = ErrorInvalidRequest
		return response
	}
	if request.Action == ActionPrepare && (request.Scope == "" || !ticketPattern.MatchString(request.EnrollmentTicket)) {
		response.Error = ErrorInvalidRequest
		return response
	}
	state, err := p.service.LocalState(ctx, p.origin)
	if err != nil {
		response.Error = errorCode(err)
		return response
	}
	response.OK = true
	result := &Result{
		InstallationID: state.InstallationID, CuaReady: state.CuaReady,
		NativeExecutorReady: state.NativeExecutorReady, GatewayConnected: state.GatewayConnected,
		RelayPaused: state.RelayPaused, OperatingSystem: "linux",
	}
	if request.Action == ActionPrepare {
		runtimeAvailable := false
		result.RuntimeAvailable = &runtimeAvailable
	}
	response.Result = result
	return response
}

func errorCode(err error) ErrorCode {
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

func hasField(fields map[string]json.RawMessage, field string) bool {
	_, ok := fields[field]
	return ok
}
