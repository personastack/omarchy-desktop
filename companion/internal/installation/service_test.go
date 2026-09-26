package installation

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"testing"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

func TestNewRequiresAPIAndCredentialStore(t *testing.T) {
	t.Parallel()
	store := &memoryStore{}
	if _, err := New(nil, store); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(nil, store) error = %v", err)
	}
	if _, err := New(&apiRecorder{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(api, nil) error = %v", err)
	}
	if _, err := New(&apiRecorder{}, store); err != nil {
		t.Fatalf("New(api, store) error = %v", err)
	}
}

func TestEnrollStoresOnlyValidatedAPIInstallation(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	api := &apiRecorder{installation: installation}
	store := &memoryStore{}
	service := mustService(t, api, store)
	if err := service.Enroll(context.Background(), "ticket", apicontract.DesktopControlOperatingSystemLinux); err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	if !reflect.DeepEqual(api.calls, []string{"enroll"}) || api.operatingSystem != apicontract.DesktopControlOperatingSystemLinux {
		t.Fatalf("API calls = %v, platform = %q", api.calls, api.operatingSystem)
	}
	if !reflect.DeepEqual(store.saved, []desktopcontrol.Installation{installation}) {
		t.Fatalf("stored installation = %#v", store.saved)
	}
}

func TestEnrollRejectsNonLinuxPlatformBeforeAPIRequest(t *testing.T) {
	t.Parallel()
	api := &apiRecorder{installation: testInstallation()}
	store := &memoryStore{}
	err := mustService(t, api, store).Enroll(context.Background(), "ticket", apicontract.DesktopControlOperatingSystemMacOS)
	if err != desktopcontrol.ErrInvalidRequest || len(api.calls) != 0 || len(store.saved) != 0 {
		t.Fatalf("Enroll() = %v, API calls %v, saved %#v", err, api.calls, store.saved)
	}
}

func TestEnrollRejectsInvalidIssuedInstallationAndMapsStoreFailure(t *testing.T) {
	t.Parallel()
	invalid := testInstallation()
	invalid.GatewayWebsocketURL = "wss://attacker.example/v1/desktop-control/ws"
	store := &memoryStore{}
	service := mustService(t, &apiRecorder{installation: invalid}, store)
	if err := service.Enroll(context.Background(), "ticket", apicontract.DesktopControlOperatingSystemLinux); !errors.Is(err, desktopcontrol.ErrUnavailable) {
		t.Fatalf("Enroll() invalid installation error = %v", err)
	}
	if len(store.saved) != 0 {
		t.Fatalf("invalid installation was stored: %#v", store.saved)
	}
	store.saveErr = errors.New("keyring failure")
	service = mustService(t, &apiRecorder{installation: testInstallation()}, store)
	if err := service.Enroll(context.Background(), "ticket", apicontract.DesktopControlOperatingSystemLinux); !errors.Is(err, credentialstore.ErrUnavailable) {
		t.Fatalf("Enroll() store error = %v", err)
	}
}

func TestEnrollPreservesAPIErrorWithoutWritingCredential(t *testing.T) {
	t.Parallel()
	apiError := errors.New("enrollment rejected")
	api := &apiRecorder{errors: map[string]error{"enroll": apiError}}
	store := &memoryStore{}
	err := mustService(t, api, store).Enroll(context.Background(), "ticket", apicontract.DesktopControlOperatingSystemLinux)
	if err != apiError || len(store.saved) != 0 {
		t.Fatalf("Enroll() error = %v, saved %#v", err, store.saved)
	}
}

func TestStatusSeparatesEnrollmentCredentialAndRelayState(t *testing.T) {
	t.Parallel()
	t.Run("not enrolled", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{}
		store := &memoryStore{loadErr: credentialstore.ErrCredentialMissing}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		if err != nil || status != (Status{}) || len(api.calls) != 0 {
			t.Fatalf("Status() = %#v, %v, API calls %v", status, err, api.calls)
		}
	})
	t.Run("active relay", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{relayActive: true}
		store := &memoryStore{loaded: installationPointer(testInstallation())}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		want := Status{Enrolled: true, CredentialValid: true, RelayActive: true}
		if err != nil || status != want || !reflect.DeepEqual(api.calls, []string{"validate", "relay-state"}) {
			t.Fatalf("Status() = %#v, %v, calls %v", status, err, api.calls)
		}
	})
	t.Run("revoked credential", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{errors: map[string]error{"validate": desktopcontrol.ErrRejected}}
		store := &memoryStore{loaded: installationPointer(testInstallation())}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		want := Status{Enrolled: true}
		if err != nil || status != want || !reflect.DeepEqual(api.calls, []string{"validate"}) {
			t.Fatalf("Status() = %#v, %v, calls %v", status, err, api.calls)
		}
	})
	t.Run("keyring failure", func(t *testing.T) {
		t.Parallel()
		store := &memoryStore{loadErr: errors.New("secret service unavailable")}
		status, err := mustService(t, &apiRecorder{}, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		if err != credentialstore.ErrUnavailable || status != (Status{}) {
			t.Fatalf("Status() = %#v, %v", status, err)
		}
	})
	t.Run("invalid stored installation", func(t *testing.T) {
		t.Parallel()
		installation := testInstallation()
		installation.GatewayWebsocketURL = "wss://attacker.example/v1/desktop-control/ws"
		api := &apiRecorder{}
		store := &memoryStore{loaded: installationPointer(installation)}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		if err != desktopcontrol.ErrUnavailable || status != (Status{}) || len(api.calls) != 0 {
			t.Fatalf("Status() = %#v, %v, API calls %v", status, err, api.calls)
		}
	})
	t.Run("credential validation unavailable", func(t *testing.T) {
		t.Parallel()
		apiError := errors.New("API unavailable")
		api := &apiRecorder{errors: map[string]error{"validate": apiError}}
		store := &memoryStore{loaded: installationPointer(testInstallation())}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		if status != (Status{Enrolled: true}) || err != apiError {
			t.Fatalf("Status() = %#v, %v", status, err)
		}
	})
	t.Run("relay status unavailable", func(t *testing.T) {
		t.Parallel()
		apiError := errors.New("API unavailable")
		api := &apiRecorder{errors: map[string]error{"relay-state": apiError}}
		store := &memoryStore{loaded: installationPointer(testInstallation())}
		status, err := mustService(t, api, store).Status(context.Background(), testInstallation().EnvironmentOrigin)
		if status != (Status{Enrolled: true, CredentialValid: true}) || err != apiError {
			t.Fatalf("Status() = %#v, %v", status, err)
		}
	})
}

func TestLocalStateReadsOnlyProtectedLocalIdentity(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	t.Run("not enrolled", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{}
		store := &memoryStore{loadErr: credentialstore.ErrCredentialMissing}
		state, err := mustService(t, api, store).LocalState(context.Background(), installation.EnvironmentOrigin)
		if err != nil || state != (LocalState{RelayPaused: true}) || len(api.calls) != 0 {
			t.Fatalf("LocalState() = %#v, %v, API calls %v", state, err, api.calls)
		}
	})
	t.Run("enrolled but runtime absent", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{}
		store := &memoryStore{loaded: installationPointer(installation)}
		state, err := mustService(t, api, store).LocalState(context.Background(), installation.EnvironmentOrigin)
		if err != nil || state.InstallationID == nil || *state.InstallationID != installation.InstallationID ||
			state.CuaReady || state.NativeExecutorReady || state.GatewayConnected || !state.RelayPaused || len(api.calls) != 0 {
			t.Fatalf("LocalState() = %#v, %v, API calls %v", state, err, api.calls)
		}
	})
	t.Run("keyring unavailable", func(t *testing.T) {
		t.Parallel()
		state, err := mustService(t, &apiRecorder{}, &memoryStore{loadErr: errors.New("secret service unavailable")}).LocalState(context.Background(), installation.EnvironmentOrigin)
		if err != credentialstore.ErrUnavailable || state != (LocalState{}) {
			t.Fatalf("LocalState() = %#v, %v", state, err)
		}
	})
}

func TestStoredInstallationKeepsCredentialInsideNativeServiceBoundary(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	service := mustService(t, &apiRecorder{}, &memoryStore{loaded: installationPointer(installation)})
	got, err := service.StoredInstallation(context.Background(), installation.EnvironmentOrigin)
	if err != nil || got != installation {
		t.Fatalf("StoredInstallation() = %#v, %v", got, err)
	}
	if _, err := service.StoredInstallation(context.Background(), "https://personastack.ericgreer.info"); !errors.Is(err, desktopcontrol.ErrInvalidRequest) {
		t.Fatalf("StoredInstallation() for a different origin = %v", err)
	}
	if _, err := service.StoredInstallation(nil, installation.EnvironmentOrigin); !errors.Is(err, credentialstore.ErrUnavailable) {
		t.Fatalf("StoredInstallation() with nil context = %v", err)
	}
}

func TestLifecycleOperationsUseStoredInstallation(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	cases := []struct {
		name string
		call func(*Service) error
		want string
	}{
		{name: "attach", call: func(s *Service) error {
			return s.Attach(context.Background(), installation.EnvironmentOrigin, "ticket")
		}, want: "attach"},
		{name: "readiness", call: func(s *Service) error {
			return s.ReportReadiness(context.Background(), installation.EnvironmentOrigin, apicontract.DesktopControlReadinessReady)
		}, want: "readiness"},
		{name: "prepare", call: func(s *Service) error {
			_, err := s.PrepareSession(context.Background(), installation.EnvironmentOrigin)
			return err
		}, want: "prepare"},
		{name: "claim", call: func(s *Service) error {
			return s.ClaimSession(context.Background(), installation.EnvironmentOrigin, "session-1", 7)
		}, want: "claim"},
		{name: "heartbeat", call: func(s *Service) error {
			return s.Heartbeat(context.Background(), installation.EnvironmentOrigin, "session-1")
		}, want: "heartbeat"},
		{name: "session readiness", call: func(s *Service) error {
			return s.ReportSessionReadiness(context.Background(), installation.EnvironmentOrigin, "session-1", apicontract.DesktopControlReadinessLocked)
		}, want: "session-readiness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &apiRecorder{generation: 7}
			store := &memoryStore{loaded: installationPointer(installation)}
			if err := tc.call(mustService(t, api, store)); err != nil {
				t.Fatalf("operation error = %v", err)
			}
			if !reflect.DeepEqual(api.calls, []string{tc.want}) {
				t.Fatalf("API calls = %v, want %s", api.calls, tc.want)
			}
			if api.lastInstallation != installation {
				t.Fatalf("API installation = %#v", api.lastInstallation)
			}
		})
	}
}

func TestLifecycleOperationsPreserveAPIErrorsAndFailClosedOnInvalidStorage(t *testing.T) {
	t.Parallel()
	apiError := errors.New("rejected by API")
	api := &apiRecorder{errors: map[string]error{"attach": apiError}}
	store := &memoryStore{loaded: installationPointer(testInstallation())}
	service := mustService(t, api, store)
	if err := service.Attach(context.Background(), testInstallation().EnvironmentOrigin, "ticket"); err != apiError {
		t.Fatalf("Attach() error = %v", err)
	}
	store.loaded = nil
	store.loadErr = credentialstore.ErrCredentialMissing
	if err := service.Heartbeat(context.Background(), testInstallation().EnvironmentOrigin, "session-1"); err != credentialstore.ErrCredentialMissing {
		t.Fatalf("Heartbeat() empty keyring error = %v", err)
	}
	store.loadErr = nil
	store.loaded = installationPointer(testInstallation())
	store.loaded.GatewayWebsocketURL = "wss://attacker.example/v1/desktop-control/ws"
	before := len(api.calls)
	if err := service.Revoke(context.Background(), testInstallation().EnvironmentOrigin); err != desktopcontrol.ErrInvalidRequest {
		t.Fatalf("Revoke() invalid stored credential error = %v", err)
	}
	if len(api.calls) != before {
		t.Fatalf("invalid credential reached API: %v", api.calls)
	}
}

func TestLifecycleOperationsPreserveAPIErrors(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	cases := []struct {
		name   string
		method string
		invoke func(*Service) error
	}{
		{name: "readiness", method: "readiness", invoke: func(s *Service) error {
			return s.ReportReadiness(context.Background(), installation.EnvironmentOrigin, apicontract.DesktopControlReadinessReady)
		}},
		{name: "prepare", method: "prepare", invoke: func(s *Service) error {
			_, err := s.PrepareSession(context.Background(), installation.EnvironmentOrigin)
			return err
		}},
		{name: "claim", method: "claim", invoke: func(s *Service) error {
			return s.ClaimSession(context.Background(), installation.EnvironmentOrigin, "session", 1)
		}},
		{name: "heartbeat", method: "heartbeat", invoke: func(s *Service) error {
			return s.Heartbeat(context.Background(), installation.EnvironmentOrigin, "session")
		}},
		{name: "session readiness", method: "session-readiness", invoke: func(s *Service) error {
			return s.ReportSessionReadiness(context.Background(), installation.EnvironmentOrigin, "session", apicontract.DesktopControlReadinessLocked)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apiError := errors.New("API request failed")
			api := &apiRecorder{errors: map[string]error{tc.method: apiError}}
			store := &memoryStore{loaded: installationPointer(installation)}
			if err := tc.invoke(mustService(t, api, store)); err != apiError {
				t.Fatalf("operation error = %v", err)
			}
			if !reflect.DeepEqual(api.calls, []string{tc.method}) {
				t.Fatalf("API calls = %v, want %s", api.calls, tc.method)
			}
		})
	}
}

func TestLifecycleOperationsDoNotReachAPIWhenCredentialStoreFails(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	cases := []struct {
		name string
		call func(*Service) error
	}{
		{name: "attach", call: func(s *Service) error {
			return s.Attach(context.Background(), installation.EnvironmentOrigin, "ticket")
		}},
		{name: "readiness", call: func(s *Service) error {
			return s.ReportReadiness(context.Background(), installation.EnvironmentOrigin, apicontract.DesktopControlReadinessReady)
		}},
		{name: "prepare", call: func(s *Service) error {
			_, err := s.PrepareSession(context.Background(), installation.EnvironmentOrigin)
			return err
		}},
		{name: "claim", call: func(s *Service) error {
			return s.ClaimSession(context.Background(), installation.EnvironmentOrigin, "session", 1)
		}},
		{name: "heartbeat", call: func(s *Service) error {
			return s.Heartbeat(context.Background(), installation.EnvironmentOrigin, "session")
		}},
		{name: "session readiness", call: func(s *Service) error {
			return s.ReportSessionReadiness(context.Background(), installation.EnvironmentOrigin, "session", apicontract.DesktopControlReadinessLocked)
		}},
		{name: "revoke", call: func(s *Service) error { return s.Revoke(context.Background(), installation.EnvironmentOrigin) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := &apiRecorder{}
			store := &memoryStore{loadErr: errors.New("keyring unavailable")}
			if err := tc.call(mustService(t, api, store)); err != credentialstore.ErrUnavailable {
				t.Fatalf("operation error = %v", err)
			}
			if len(api.calls) != 0 {
				t.Fatalf("credential-store failure reached API: %v", api.calls)
			}
		})
	}
}

func TestRevokeDeletesCredentialOnlyAfterAPIRevokes(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	t.Run("API rejects", func(t *testing.T) {
		t.Parallel()
		apiError := errors.New("API unavailable")
		api := &apiRecorder{errors: map[string]error{"revoke": apiError}}
		store := &memoryStore{loaded: installationPointer(installation)}
		if err := mustService(t, api, store).Revoke(context.Background(), installation.EnvironmentOrigin); err != apiError {
			t.Fatalf("Revoke() error = %v", err)
		}
		if len(store.deleted) != 0 {
			t.Fatalf("credential deleted after failed API revoke: %#v", store.deleted)
		}
	})
	t.Run("keyring cleanup fails", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{}
		store := &memoryStore{loaded: installationPointer(installation), deleteErr: errors.New("keyring unavailable")}
		if err := mustService(t, api, store).Revoke(context.Background(), installation.EnvironmentOrigin); err != credentialstore.ErrUnavailable {
			t.Fatalf("Revoke() error = %v", err)
		}
		if !reflect.DeepEqual(api.calls, []string{"revoke"}) || store.deleteAttempts != 1 || len(store.deleted) != 0 {
			t.Fatalf("API calls %v, delete attempts %d, deleted %#v", api.calls, store.deleteAttempts, store.deleted)
		}
	})
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		api := &apiRecorder{}
		store := &memoryStore{loaded: installationPointer(installation)}
		if err := mustService(t, api, store).Revoke(context.Background(), installation.EnvironmentOrigin); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		if !reflect.DeepEqual(api.calls, []string{"revoke"}) || !reflect.DeepEqual(store.deleted, []desktopcontrol.Installation{installation}) {
			t.Fatalf("API calls %v, deleted %#v", api.calls, store.deleted)
		}
	})
}

type apiRecorder struct {
	calls            []string
	installation     desktopcontrol.Installation
	lastInstallation desktopcontrol.Installation
	operatingSystem  apicontract.DesktopControlOperatingSystem
	relayActive      bool
	generation       int64
	errors           map[string]error
}

func (a *apiRecorder) record(name string, installation desktopcontrol.Installation) error {
	a.calls = append(a.calls, name)
	a.lastInstallation = installation
	return a.errors[name]
}

func (a *apiRecorder) Enroll(_ context.Context, _ string, operatingSystem apicontract.DesktopControlOperatingSystem) (desktopcontrol.Installation, error) {
	a.calls = append(a.calls, "enroll")
	a.operatingSystem = operatingSystem
	return a.installation, a.errors["enroll"]
}

func (a *apiRecorder) Attach(_ context.Context, installation desktopcontrol.Installation, _ string) error {
	return a.record("attach", installation)
}

func (a *apiRecorder) Validate(_ context.Context, installation desktopcontrol.Installation) error {
	return a.record("validate", installation)
}

func (a *apiRecorder) ReadRelayState(_ context.Context, installation desktopcontrol.Installation) (bool, error) {
	if err := a.record("relay-state", installation); err != nil {
		return false, err
	}
	return a.relayActive, nil
}

func (a *apiRecorder) ReportReadiness(_ context.Context, installation desktopcontrol.Installation, _ apicontract.DesktopControlReadiness) error {
	return a.record("readiness", installation)
}

func (a *apiRecorder) PrepareSession(_ context.Context, installation desktopcontrol.Installation) (int64, error) {
	if err := a.record("prepare", installation); err != nil {
		return 0, err
	}
	return a.generation, nil
}

func (a *apiRecorder) ClaimSession(_ context.Context, installation desktopcontrol.Installation, _ string, _ int64) error {
	return a.record("claim", installation)
}

func (a *apiRecorder) Heartbeat(_ context.Context, installation desktopcontrol.Installation, _ string) error {
	return a.record("heartbeat", installation)
}

func (a *apiRecorder) ReportSessionReadiness(_ context.Context, installation desktopcontrol.Installation, _ string, _ apicontract.DesktopControlReadiness) error {
	return a.record("session-readiness", installation)
}

func (a *apiRecorder) Revoke(_ context.Context, installation desktopcontrol.Installation) error {
	return a.record("revoke", installation)
}

type memoryStore struct {
	saved          []desktopcontrol.Installation
	deleted        []desktopcontrol.Installation
	deleteAttempts int
	loaded         *desktopcontrol.Installation
	saveErr        error
	loadErr        error
	deleteErr      error
}

func (s *memoryStore) Save(_ context.Context, installation desktopcontrol.Installation) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saved = append(s.saved, installation)
	return nil
}

func (s *memoryStore) Load(_ context.Context, _ string) (*desktopcontrol.Installation, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.loaded == nil {
		return nil, nil
	}
	copy := *s.loaded
	return &copy, nil
}

func (s *memoryStore) Delete(_ context.Context, installation desktopcontrol.Installation) error {
	s.deleteAttempts++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, installation)
	return nil
}

func mustService(t *testing.T, api API, store Store) *Service {
	t.Helper()
	service, err := New(api, store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testInstallation() desktopcontrol.Installation {
	return desktopcontrol.Installation{
		EnvironmentOrigin:   "https://my.personastack.ai",
		InstallationID:      "install_01",
		MachineCredential:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		GatewayWebsocketURL: "wss://cluster-agent.personastack.ai/v1/desktop-control/ws",
	}
}

func installationPointer(installation desktopcontrol.Installation) *desktopcontrol.Installation {
	return &installation
}
