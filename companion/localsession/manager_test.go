package localsession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

func TestManagerRunsStateSelectPrepareConfigureJourney(t *testing.T) {
	t.Parallel()
	fixture := newProbeFixture(t, HarnessCodex, "codex-cli 0.154.0", "add", "")
	preferences := &preferenceStub{}
	installer := &installerStub{}
	manager, err := NewManager(fixture.probe, installer, preferences)
	if err != nil {
		t.Fatal(err)
	}
	manager.SynchronizeScope("workspace:one")
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	manager.newID = func() (string, error) { return "c725451f-2d11-4e46-adfd-e92f2fc84c01", nil }
	origin := "https://my.personastack.ai"
	scope := "workspace:one"
	state, err := manager.Handle(context.Background(), origin, Command{Action: ActionState, Scope: scope})
	if err != nil || !strings.Contains(string(state), `"version":"2"`) {
		t.Fatalf("state = %s, %v", state, err)
	}
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionSelect, Scope: scope, Harness: HarnessCodex}); err != nil {
		t.Fatalf("select = %v", err)
	}
	if preferences.values[origin] != HarnessCodex {
		t.Fatalf("selected harness = %q", preferences.values[origin])
	}
	prepared, err := manager.Handle(context.Background(), origin, Command{Action: ActionPrepare, Scope: scope, Harness: HarnessCodex, PersonaID: "persona_one"})
	if err != nil {
		t.Fatalf("prepare = %s, %v", prepared, err)
	}
	var preparedResponse response
	if err := json.Unmarshal(prepared, &preparedResponse); err != nil || !preparedResponse.OK || preparedResponse.PendingID == "" {
		t.Fatalf("prepare response = %s, %v", prepared, err)
	}
	bundle := validBundle(now)
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: scope, PendingID: preparedResponse.PendingID, Bundle: raw})
	if err != nil {
		t.Fatalf("configure = %s, %v", configured, err)
	}
	if !strings.Contains(string(configured), `"ok":true`) || installer.configures != 1 || installer.origin != origin {
		t.Fatalf("configure = %s, installer = %#v", configured, installer)
	}
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: scope, PendingID: preparedResponse.PendingID, Bundle: raw}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("duplicate configure error = %v", err)
	}
	if installer.configures != 1 {
		t.Fatalf("duplicate configure mutated installer %d times", installer.configures)
	}
}

func TestManagerInvalidatesPendingOnScopeChangeAndExpiry(t *testing.T) {
	t.Parallel()
	fixture := newProbeFixture(t, HarnessCodex, "0.154.0", "add", "")
	installer := &installerStub{}
	manager, err := NewManager(fixture.probe, installer, &preferenceStub{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	manager.newID = func() (string, error) { return "c725451f-2d11-4e46-adfd-e92f2fc84c01", nil }
	origin := "https://my.personastack.ai"
	manager.SynchronizeScope("one")
	_, _ = manager.Handle(context.Background(), origin, Command{Action: ActionState, Scope: "one"})
	prepared, err := manager.Handle(context.Background(), origin, Command{Action: ActionPrepare, Scope: "one", Harness: HarnessCodex, PersonaID: "persona_one"})
	if err != nil {
		t.Fatal(err)
	}
	var pending response
	_ = json.Unmarshal(prepared, &pending)
	manager.SynchronizeScope("two")
	bundle, _ := json.Marshal(validBundle(now))
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: "one", PendingID: pending.PendingID, Bundle: bundle}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("scope changed configure error = %v", err)
	}
	manager.SynchronizeScope("one")
	_, _ = manager.Handle(context.Background(), origin, Command{Action: ActionState, Scope: "one"})
	prepared, err = manager.Handle(context.Background(), origin, Command{Action: ActionPrepare, Scope: "one", Harness: HarnessCodex, PersonaID: "persona_one"})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(prepared, &pending)
	now = now.Add(pendingLifetime)
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: "one", PendingID: pending.PendingID, Bundle: bundle}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("expired configure error = %v", err)
	}
	if installer.configures != 0 {
		t.Fatalf("stale requests reached installer %d times", installer.configures)
	}
}

func TestManagerRejectsLateStateFromPreviousScopeWithoutClearingCurrentPendingWork(t *testing.T) {
	t.Parallel()
	fixture := newProbeFixture(t, HarnessCodex, "codex-cli 0.154.0", "add", "")
	installer := &installerStub{}
	manager, err := NewManager(fixture.probe, installer, &preferenceStub{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	manager.newID = func() (string, error) { return "c725451f-2d11-4e46-adfd-e92f2fc84c01", nil }
	origin := "https://my.personastack.ai"
	manager.SynchronizeScope("workspace:old")
	manager.SynchronizeScope("workspace:new")
	prepared, err := manager.Handle(context.Background(), origin, Command{Action: ActionPrepare, Scope: "workspace:new", Harness: HarnessCodex, PersonaID: "persona_one"})
	if err != nil {
		t.Fatal(err)
	}
	var pending response
	if err := json.Unmarshal(prepared, &pending); err != nil || pending.PendingID == "" {
		t.Fatalf("current scope pending = %s, %v", prepared, err)
	}
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionState, Scope: "workspace:old"}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("late old-scope state error = %v", err)
	}
	bundle, err := json.Marshal(validBundle(now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: "workspace:new", PendingID: pending.PendingID, Bundle: bundle}); err != nil {
		t.Fatalf("current scope configure after stale read = %v", err)
	}
	if installer.configures != 1 {
		t.Fatalf("current scope installer calls = %d", installer.configures)
	}
}

func TestManagerConfigureRequiresMatchingPersonaAndTypedJSON(t *testing.T) {
	t.Parallel()
	fixture := newProbeFixture(t, HarnessCodex, "0.154.0", "add", "")
	installer := &installerStub{}
	manager, err := NewManager(fixture.probe, installer, &preferenceStub{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	manager.newID = func() (string, error) { return "c725451f-2d11-4e46-adfd-e92f2fc84c01", nil }
	origin := "https://my.personastack.ai"
	manager.SynchronizeScope("one")
	_, _ = manager.Handle(context.Background(), origin, Command{Action: ActionState, Scope: "one"})
	prepared, err := manager.Handle(context.Background(), origin, Command{Action: ActionPrepare, Scope: "one", Harness: HarnessCodex, PersonaID: "persona_one"})
	if err != nil {
		t.Fatal(err)
	}
	var pending response
	_ = json.Unmarshal(prepared, &pending)
	bundle := validBundle(now)
	bundle.PersonaID = "persona_two"
	raw, _ := json.Marshal(bundle)
	if _, err := manager.Handle(context.Background(), origin, Command{Action: ActionConfigure, Scope: "one", PendingID: pending.PendingID, Bundle: raw}); !errors.Is(err, ErrStaleRequest) {
		t.Fatalf("wrong persona error = %v", err)
	}
	if _, err := parseBundle([]byte(`{"persona_id":"p","unknown":true}`)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("unknown bundle property error = %v", err)
	}
	missingArray := strings.Replace(string(raw), `"skills":[`, `"skills":null`, 1)
	if _, err := parseBundle([]byte(missingArray)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("null skills error = %v", err)
	}
	if installer.configures != 0 {
		t.Fatalf("invalid requests reached installer %d times", installer.configures)
	}
}

func TestFilePreferencesPersistsOnlyValidOriginScopedHarness(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "local-session-preferences.json")
	preferences := FilePreferences{path: path}
	if err := preferences.Save(context.Background(), "https://my.personastack.ai", HarnessCodex); err != nil {
		t.Fatal(err)
	}
	if err := preferences.Save(context.Background(), "https://personastack.ericgreer.info", HarnessClaudeCode); err != nil {
		t.Fatal(err)
	}
	production, err := preferences.Load(context.Background(), "https://my.personastack.ai")
	if err != nil || production != HarnessCodex {
		t.Fatalf("production preference = %q, %v", production, err)
	}
	lan, err := preferences.Load(context.Background(), "https://personastack.ericgreer.info")
	if err != nil || lan != HarnessClaudeCode {
		t.Fatalf("LAN preference = %q, %v", lan, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("preference file mode = %v, %v", info, err)
	}
	if err := preferences.Save(context.Background(), "https://attacker.example", HarnessCodex); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid origin save error = %v", err)
	}
}

func TestFilePreferencesConcurrentOriginsRetainBothSelections(t *testing.T) {
	t.Parallel()
	preferences := FilePreferences{path: filepath.Join(t.TempDir(), "local-session-preferences.json")}
	const writes = 32
	var workers sync.WaitGroup
	results := make(chan error, 2*writes)
	for index := 0; index < writes; index++ {
		workers.Add(2)
		go func() {
			defer workers.Done()
			results <- preferences.Save(context.Background(), "https://my.personastack.ai", HarnessCodex)
		}()
		go func() {
			defer workers.Done()
			results <- preferences.Save(context.Background(), "https://personastack.ericgreer.info", HarnessClaudeCode)
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent preference save = %v", err)
		}
	}
	for origin, want := range map[string]string{
		"https://my.personastack.ai":          HarnessCodex,
		"https://personastack.ericgreer.info": HarnessClaudeCode,
	} {
		got, err := preferences.Load(context.Background(), origin)
		if err != nil || got != want {
			t.Fatalf("preference %q = %q, %v; want %q", origin, got, err, want)
		}
	}
}

func TestFilePreferencesRejectsSymlinkAndBroadMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	path := filepath.Join(root, "local-session-preferences.json")
	if err := os.WriteFile(secret, []byte(`{"https://my.personastack.ai":"codex"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Fatal(err)
	}
	preferences := FilePreferences{path: path}
	if _, err := preferences.Load(context.Background(), "https://my.personastack.ai"); err == nil {
		t.Fatal("loaded preferences through a symlink")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(secret, path); err != nil {
		t.Fatal(err)
	}
	if _, err := preferences.Load(context.Background(), "https://my.personastack.ai"); err == nil {
		t.Fatal("loaded broad-mode preferences")
	}
}

type preferenceStub struct{ values map[string]string }

func (s *preferenceStub) Load(_ context.Context, origin string) (string, error) {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	return s.values[origin], nil
}

func (s *preferenceStub) Save(_ context.Context, origin, harness string) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[origin] = harness
	return nil
}

type installerStub struct {
	configures int
	origin     string
}

func (*installerStub) Preflight(context.Context, string, HarnessInstallation) error { return nil }

func (s *installerStub) Configure(_ context.Context, _ apicontract.LocalSessionResponse, origin, _ string, _ HarnessInstallation) error {
	s.configures++
	s.origin = origin
	return nil
}
