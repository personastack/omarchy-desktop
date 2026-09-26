package desktopbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const MaxRequestBytes = 4 * 1024
const maxRequestID = 1<<53 - 1

var ErrInvalidRequest = errors.New("invalid desktop bridge request")

type Action string

const (
	ActionStatus Action = "status"
	ActionEnroll Action = "enroll"
	ActionAttach Action = "attach"
	ActionRevoke Action = "revoke"
)

type Request struct {
	ID     uint64
	Action Action
	Ticket string
}

type ErrorCode string

const (
	ErrorInvalidRequest     ErrorCode = "invalid_request"
	ErrorNotEnrolled        ErrorCode = "not_enrolled"
	ErrorKeyringUnavailable ErrorCode = "keyring_unavailable"
	ErrorRejected           ErrorCode = "rejected"
	ErrorUnavailable        ErrorCode = "unavailable"
)

type Response struct {
	ID     uint64               `json:"id"`
	OK     bool                 `json:"ok"`
	Error  ErrorCode            `json:"error,omitempty"`
	Status *installation.Status `json:"status,omitempty"`
}

type Service interface {
	Enroll(context.Context, string, apicontract.DesktopControlOperatingSystem) error
	Attach(context.Context, string, string) error
	Status(context.Context, string) (installation.Status, error)
	Revoke(context.Context, string) error
}

type Processor struct {
	service Service
	origin  string
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
	switch request.Action {
	case ActionStatus, ActionRevoke:
		if len(fields) != 2 {
			return Request{}, ErrInvalidRequest
		}
	case ActionEnroll, ActionAttach:
		if len(fields) != 3 || strings.TrimSpace(request.Ticket) == "" || len(request.Ticket) > 128 {
			return Request{}, ErrInvalidRequest
		}
	default:
		return Request{}, ErrInvalidRequest
	}
	if _, ok := fields["id"]; !ok {
		return Request{}, ErrInvalidRequest
	}
	if _, ok := fields["action"]; !ok {
		return Request{}, ErrInvalidRequest
	}
	if (request.Action == ActionEnroll || request.Action == ActionAttach) != hasField(fields, "ticket") {
		return Request{}, ErrInvalidRequest
	}
	return request, nil
}

func (p *Processor) Handle(ctx context.Context, request Request) Response {
	response := Response{ID: request.ID}
	var err error
	switch request.Action {
	case ActionStatus:
		var status installation.Status
		status, err = p.service.Status(ctx, p.origin)
		if err == nil {
			response.Status = &status
		}
	case ActionEnroll:
		err = p.service.Enroll(ctx, request.Ticket, apicontract.DesktopControlOperatingSystemLinux)
	case ActionAttach:
		err = p.service.Attach(ctx, p.origin, request.Ticket)
	case ActionRevoke:
		err = p.service.Revoke(ctx, p.origin)
	default:
		err = ErrInvalidRequest
	}
	if err != nil {
		response.Error = errorCode(err)
		return response
	}
	response.OK = true
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
