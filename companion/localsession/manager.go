package localsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

const pendingLifetime = 5 * time.Minute
const maxPendingRequests = 64

var originPattern = regexp.MustCompile(`^https://(my\.personastack\.ai|personastack\.ericgreer\.info)$`)

var ErrStaleRequest = errors.New("local session request is stale")

var preferenceFileMu sync.Mutex

type Installer interface {
	Preflight(context.Context, string, HarnessInstallation) error
	Configure(context.Context, apicontract.LocalSessionResponse, string, string, HarnessInstallation) error
}

type PreferenceStore interface {
	Load(context.Context, string) (string, error)
	Save(context.Context, string, string) error
}

type Pending struct {
	ID      string
	Scope   string
	Persona string
	Harness string
	Expires time.Time
}

type response struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version,omitempty"`
	Harness   string `json:"harness,omitempty"`
	PendingID string `json:"pending_id,omitempty"`
}

type Manager struct {
	mu          sync.Mutex
	probe       Probe
	installer   Installer
	preferences PreferenceStore
	now         func() time.Time
	newID       func() (string, error)
	scope       string
	generation  uint64
	pending     map[string]Pending
	profiles    map[string]bool
}

func NewManager(probe Probe, installer Installer, preferences PreferenceStore) (*Manager, error) {
	if probe.run == nil || probe.environment == nil || probe.shell == nil || probe.isExecutable == nil || installer == nil || preferences == nil {
		return nil, ErrInvalidRequest
	}
	return &Manager{
		probe: probe, installer: installer, preferences: preferences, now: time.Now, newID: newPendingID,
		pending: make(map[string]Pending), profiles: make(map[string]bool),
	}, nil
}

// Handle applies the finite hosted local-session contract. A manager belongs to
// one registered top-level page; the caller supplies its already-admitted origin.
func (m *Manager) Handle(ctx context.Context, origin string, command Command) (json.RawMessage, error) {
	if !originPattern.MatchString(origin) || len(command.Scope) > 512 || (command.Scope == "" && command.Action != ActionState) {
		return nil, ErrInvalidRequest
	}
	switch command.Action {
	case ActionState:
		return m.state(ctx, origin, command.Scope)
	case ActionSelect:
		return m.selectHarness(ctx, origin, command)
	case ActionPrepare:
		return m.prepare(ctx, origin, command)
	case ActionConfigure:
		return m.configure(ctx, origin, command)
	default:
		return nil, ErrInvalidRequest
	}
}

func (m *Manager) state(ctx context.Context, origin, scope string) (json.RawMessage, error) {
	m.mu.Lock()
	m.resetScope(scope)
	m.mu.Unlock()
	harness, err := m.preferences.Load(ctx, origin)
	if err != nil {
		return nil, ErrUnavailable
	}
	if harness != "" && !validHarness(harness) {
		return nil, ErrUnavailable
	}
	result := response{OK: true, Version: "2", Harness: harness}
	return json.Marshal(result)
}

func (m *Manager) selectHarness(ctx context.Context, origin string, command Command) (json.RawMessage, error) {
	if !validHarness(command.Harness) {
		return nil, ErrInvalidRequest
	}
	m.mu.Lock()
	if m.scope != command.Scope {
		m.mu.Unlock()
		return nil, ErrStaleRequest
	}
	m.mu.Unlock()
	if err := m.preferences.Save(ctx, origin, command.Harness); err != nil {
		return nil, ErrUnavailable
	}
	return json.Marshal(response{OK: true})
}

func (m *Manager) prepare(ctx context.Context, origin string, command Command) (json.RawMessage, error) {
	if !validHarness(command.Harness) || !personaIDPattern.MatchString(command.PersonaID) {
		return nil, ErrInvalidRequest
	}
	m.mu.Lock()
	if m.scope != command.Scope {
		m.mu.Unlock()
		return nil, ErrStaleRequest
	}
	generation := m.generation
	m.mu.Unlock()
	installation, err := m.probe.Inspect(ctx, command.Harness)
	if err != nil {
		return nil, err
	}
	if err := m.installer.Preflight(ctx, command.Harness, installation); err != nil {
		return nil, ErrUnsafeFiles
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scope != command.Scope || m.generation != generation {
		return nil, ErrStaleRequest
	}
	id, err := m.newID()
	if err != nil {
		return nil, ErrUnavailable
	}
	now := m.now()
	for pendingID, candidate := range m.pending {
		if !now.Before(candidate.Expires) {
			delete(m.pending, pendingID)
		}
	}
	if len(m.pending) >= maxPendingRequests {
		return nil, ErrUnavailable
	}
	m.pending[id] = Pending{ID: id, Scope: command.Scope, Persona: command.PersonaID, Harness: command.Harness, Expires: now.Add(pendingLifetime)}
	return json.Marshal(response{OK: true, PendingID: id})
}

func (m *Manager) configure(ctx context.Context, origin string, command Command) (json.RawMessage, error) {
	bundle, err := parseBundle(command.Bundle)
	if err != nil || ValidateBundle(bundle, origin, m.now()) != nil {
		return nil, ErrInvalidBundle
	}
	m.mu.Lock()
	pending, exists := m.pending[command.PendingID]
	if !exists || pending.Scope != command.Scope || !m.now().Before(pending.Expires) || pending.Harness != string(bundle.Harness) || pending.Persona != bundle.PersonaID {
		delete(m.pending, command.PendingID)
		m.mu.Unlock()
		return nil, ErrStaleRequest
	}
	delete(m.pending, command.PendingID)
	generation := m.generation
	m.mu.Unlock()
	installation, err := m.probe.Inspect(ctx, string(bundle.Harness))
	if err != nil {
		return nil, err
	}
	profileKey, err := filepath.EvalSymlinks(installation.Profile)
	if err != nil {
		return nil, ErrUnsafeFiles
	}
	profileKey = filepath.Clean(profileKey)
	m.mu.Lock()
	if m.scope != command.Scope || m.generation != generation || m.profiles[profileKey] {
		m.mu.Unlock()
		return nil, ErrStaleRequest
	}
	m.profiles[profileKey] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.profiles, profileKey)
		m.mu.Unlock()
	}()
	if err := m.installer.Configure(ctx, bundle, origin, pending.ID, installation); err != nil {
		return nil, ErrUnsafeFiles
	}
	return json.Marshal(response{OK: true})
}

func (m *Manager) resetScope(scope string) {
	if m.scope == scope {
		return
	}
	m.scope = scope
	m.generation++
	m.pending = make(map[string]Pending)
}

func parseBundle(raw []byte) (apicontract.LocalSessionResponse, error) {
	var bundle apicontract.LocalSessionResponse
	if len(raw) == 0 || len(raw) > apicontract.LocalSessionMaxWireBytes || !wirejson.ValidUniqueJSON(raw) {
		return bundle, ErrInvalidBundle
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || !exactJSONFields(object, "persona_id", "persona_name", "workspace_id", "harness", "issued_at", "expires_at", "mcp_url", "bearer_token", "persona_prompt", "skills") {
		return apicontract.LocalSessionResponse{}, ErrInvalidBundle
	}
	if !jsonArray(object["skills"]) {
		return apicontract.LocalSessionResponse{}, ErrInvalidBundle
	}
	var skills []map[string]json.RawMessage
	if err := json.Unmarshal(object["skills"], &skills); err != nil {
		return apicontract.LocalSessionResponse{}, ErrInvalidBundle
	}
	for _, skill := range skills {
		if !exactJSONFields(skill, "skill_id", "slug", "digest", "files") {
			return apicontract.LocalSessionResponse{}, ErrInvalidBundle
		}
		if !jsonArray(skill["files"]) {
			return apicontract.LocalSessionResponse{}, ErrInvalidBundle
		}
		var files []map[string]json.RawMessage
		if err := json.Unmarshal(skill["files"], &files); err != nil {
			return apicontract.LocalSessionResponse{}, ErrInvalidBundle
		}
		for _, file := range files {
			if !exactJSONFields(file, "relative_path", "content") {
				return apicontract.LocalSessionResponse{}, ErrInvalidBundle
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return apicontract.LocalSessionResponse{}, ErrInvalidBundle
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return apicontract.LocalSessionResponse{}, ErrInvalidBundle
	}
	return bundle, nil
}

func jsonArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func exactJSONFields(fields map[string]json.RawMessage, expected ...string) bool {
	if fields == nil || len(fields) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, exists := fields[key]; !exists {
			return false
		}
	}
	return true
}

func newPendingID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate local session identifier: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
	return encoded, nil
}

// FilePreferences stores only the selected harness. The Linux OS credential
// store remains the sole owner of machine credentials.
type FilePreferences struct{ path string }

func NewFilePreferences() (FilePreferences, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return FilePreferences{}, ErrUnavailable
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return FilePreferences{}, ErrUnavailable
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return FilePreferences{}, ErrUnavailable
	}
	directory := filepath.Join(root, "personastack")
	err = os.Mkdir(directory, 0o700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return FilePreferences{}, ErrUnavailable
	}
	if err := verifyPrivateDirectory(directory); err != nil {
		return FilePreferences{}, ErrUnavailable
	}
	return FilePreferences{path: filepath.Join(directory, "local-session-preferences.json")}, nil
}

func (p FilePreferences) Load(ctx context.Context, origin string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !originPattern.MatchString(origin) || p.path == "" {
		return "", ErrInvalidRequest
	}
	preferenceFileMu.Lock()
	defer preferenceFileMu.Unlock()
	preferences, err := p.read()
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return preferences[origin], nil
}

func (p FilePreferences) Save(ctx context.Context, origin, harness string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !originPattern.MatchString(origin) || !validHarness(harness) || p.path == "" {
		return ErrInvalidRequest
	}
	preferenceFileMu.Lock()
	defer preferenceFileMu.Unlock()
	preferences, err := p.read()
	if errors.Is(err, os.ErrNotExist) {
		preferences = make(map[string]string)
	} else if err != nil {
		return err
	}
	preferences[origin] = harness
	data, err := json.Marshal(preferences)
	if err != nil {
		return ErrUnavailable
	}
	info, err := os.Lstat(p.path)
	if err == nil && verifyPrivateFileInfo(info) != nil {
		return ErrUnavailable
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnavailable
	}
	file, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrUnavailable
	}
	if info, err := file.Stat(); err != nil || verifyPrivateFileInfo(info) != nil {
		file.Close()
		return ErrUnavailable
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}

func (p FilePreferences) read() (map[string]string, error) {
	if p.path == "" {
		return nil, ErrUnavailable
	}
	file, err := os.OpenFile(p.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || verifyPrivateFileInfo(info) != nil {
		return nil, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 || !wirejson.ValidUniqueJSON(data) {
		return nil, ErrUnavailable
	}
	preferences := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&preferences); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrUnavailable
	}
	for origin, harness := range preferences {
		if !originPattern.MatchString(origin) || !validHarness(harness) {
			return nil, ErrUnavailable
		}
	}
	return preferences, nil
}

func verifyPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrUnavailable
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return ErrUnavailable
	}
	return nil
}

func verifyPrivateFileInfo(info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return ErrUnavailable
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || owner.Nlink != 1 {
		return ErrUnavailable
	}
	return nil
}
