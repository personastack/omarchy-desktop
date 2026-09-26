package installation

import (
	"context"
	"errors"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

var ErrUnavailable = errors.New("desktop installation service unavailable")

type API interface {
	Enroll(context.Context, string, apicontract.DesktopControlOperatingSystem) (desktopcontrol.Installation, error)
	Attach(context.Context, desktopcontrol.Installation, string) error
	Validate(context.Context, desktopcontrol.Installation) error
	ReadRelayState(context.Context, desktopcontrol.Installation) (bool, error)
	ReportReadiness(context.Context, desktopcontrol.Installation, apicontract.DesktopControlReadiness) error
	PrepareSession(context.Context, desktopcontrol.Installation) (int64, error)
	ClaimSession(context.Context, desktopcontrol.Installation, string, int64) error
	Heartbeat(context.Context, desktopcontrol.Installation, string) error
	ReportSessionReadiness(context.Context, desktopcontrol.Installation, string, apicontract.DesktopControlReadiness) error
	Revoke(context.Context, desktopcontrol.Installation) error
}

type Store interface {
	Save(context.Context, desktopcontrol.Installation) error
	Load(context.Context, string) (*desktopcontrol.Installation, error)
	Delete(context.Context, desktopcontrol.Installation) error
}

type Service struct {
	api   API
	store Store
}

type Status struct {
	Enrolled        bool `json:"enrolled"`
	CredentialValid bool `json:"credential_valid"`
	RelayActive     bool `json:"relay_active"`
}

func New(api API, store Store) (*Service, error) {
	if api == nil || store == nil {
		return nil, ErrUnavailable
	}
	return &Service{api: api, store: store}, nil
}

func (s *Service) Enroll(ctx context.Context, ticket string, operatingSystem apicontract.DesktopControlOperatingSystem) error {
	if operatingSystem != apicontract.DesktopControlOperatingSystemLinux {
		return desktopcontrol.ErrInvalidRequest
	}
	installation, err := s.api.Enroll(ctx, ticket, operatingSystem)
	if err != nil {
		return err
	}
	if err := installation.Validate(installation.EnvironmentOrigin); err != nil {
		return desktopcontrol.ErrUnavailable
	}
	if err := s.store.Save(ctx, installation); err != nil {
		return credentialstore.ErrUnavailable
	}
	return nil
}

func (s *Service) Attach(ctx context.Context, origin, ticket string) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	return s.api.Attach(ctx, installation, ticket)
}

func (s *Service) Status(ctx context.Context, origin string) (Status, error) {
	installation, err := s.store.Load(ctx, origin)
	if errors.Is(err, credentialstore.ErrCredentialMissing) {
		return Status{}, nil
	}
	if err != nil || installation == nil {
		return Status{}, credentialstore.ErrUnavailable
	}
	if err := installation.Validate(origin); err != nil {
		return Status{}, desktopcontrol.ErrUnavailable
	}
	status := Status{Enrolled: true}
	if err := s.api.Validate(ctx, *installation); err != nil {
		if errors.Is(err, desktopcontrol.ErrRejected) {
			return status, nil
		}
		return status, err
	}
	status.CredentialValid = true
	active, err := s.api.ReadRelayState(ctx, *installation)
	if err != nil {
		return status, err
	}
	status.RelayActive = active
	return status, nil
}

func (s *Service) ReportReadiness(ctx context.Context, origin string, readiness apicontract.DesktopControlReadiness) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	return s.api.ReportReadiness(ctx, installation, readiness)
}

func (s *Service) PrepareSession(ctx context.Context, origin string) (int64, error) {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return 0, err
	}
	return s.api.PrepareSession(ctx, installation)
}

func (s *Service) ClaimSession(ctx context.Context, origin, sessionID string, generation int64) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	return s.api.ClaimSession(ctx, installation, sessionID, generation)
}

func (s *Service) Heartbeat(ctx context.Context, origin, sessionID string) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	return s.api.Heartbeat(ctx, installation, sessionID)
}

func (s *Service) ReportSessionReadiness(ctx context.Context, origin, sessionID string, readiness apicontract.DesktopControlReadiness) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	return s.api.ReportSessionReadiness(ctx, installation, sessionID, readiness)
}

func (s *Service) Revoke(ctx context.Context, origin string) error {
	installation, err := s.load(ctx, origin)
	if err != nil {
		return err
	}
	if err := s.api.Revoke(ctx, installation); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, installation); err != nil {
		return credentialstore.ErrUnavailable
	}
	return nil
}

func (s *Service) load(ctx context.Context, origin string) (desktopcontrol.Installation, error) {
	installation, err := s.store.Load(ctx, origin)
	if errors.Is(err, credentialstore.ErrCredentialMissing) {
		return desktopcontrol.Installation{}, credentialstore.ErrCredentialMissing
	}
	if err != nil || installation == nil {
		return desktopcontrol.Installation{}, credentialstore.ErrUnavailable
	}
	if err := installation.Validate(origin); err != nil {
		return desktopcontrol.Installation{}, desktopcontrol.ErrInvalidRequest
	}
	return *installation, nil
}
