//go:build linux

package localsession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

func TestFileInstallerPreflightIsReadOnly(t *testing.T) {
	t.Parallel()
	installation, root := newInstallerInstallation(t, HarnessCodex)
	dataRoot := filepath.Join(root, "data", "share")
	runner := &pluginRunner{harness: HarnessCodex, profile: installation.Profile}
	installer, err := NewFileInstaller(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	installer.run = runner.run
	if err := installer.Preflight(context.Background(), HarnessCodex, installation); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if _, err := os.Lstat(dataRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created data root: %v", err)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "plugin marketplace list --json" {
		t.Fatalf("preflight CLI calls = %#v", runner.calls)
	}
}

func TestFileInstallerConfiguresCodexAndClaudeThroughPluginManagers(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{HarnessCodex, HarnessClaudeCode} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			installation, root := newInstallerInstallation(t, harness)
			dataRoot := filepath.Join(root, "xdg", "share")
			installer, err := NewFileInstaller(dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			runner := &pluginRunner{harness: harness, profile: installation.Profile, marketplaceName: installerMarketName(t, installer, harness, installation)}
			installer.run = runner.run
			if err := installer.Preflight(context.Background(), harness, installation); err != nil {
				t.Fatalf("Preflight() error = %v", err)
			}
			now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
			bundle := validBundle(now)
			if harness == HarnessClaudeCode {
				bundle.Harness = apicontract.LocalSessionHarnessClaudeCode
			}
			if err := installer.Configure(context.Background(), bundle, "https://my.personastack.ai", "c725451f-2d11-4e46-adfd-e92f2fc84c01", installation); err != nil {
				t.Fatalf("Configure() error = %v", err)
			}
			if !runner.installed || len(runner.calls) < 4 {
				t.Fatalf("plugin install was not completed: calls=%#v", runner.calls)
			}
			installCall := strings.Join(runner.calls[3], " ")
			want := "plugin add "
			if harness == HarnessClaudeCode {
				want = "plugin install "
			}
			if !strings.HasPrefix(installCall, want) {
				t.Fatalf("plugin install call = %q, want prefix %q", installCall, want)
			}
			paths, err := installer.paths(harness, installation)
			if err != nil {
				t.Fatal(err)
			}
			active, err := readActive(paths)
			if err != nil || active == nil {
				t.Fatalf("active plugin ownership = %#v, %v", active, err)
			}
			pluginSource := filepath.Join(active.Source, "marketplace", "plugins", paths.pluginName)
			mcpPath := filepath.Join(pluginSource, ".mcp.json")
			mcpInfo, err := os.Stat(mcpPath)
			if err != nil || mcpInfo.Mode().Perm() != 0o600 {
				t.Fatalf("MCP file mode = %v, %v", mcpInfo, err)
			}
			mcpBytes, err := os.ReadFile(mcpPath)
			if err != nil || !strings.Contains(string(mcpBytes), bundle.BearerToken) {
				t.Fatal("plugin MCP file did not contain the API-issued credential")
			}
			cachePath := filepath.Join(installation.Profile, "plugins", "cache", paths.marketplaceName, paths.pluginName, "1.0.0")
			assertPrivateTree(t, cachePath)
			if strings.Contains(string(mcpBytes), "OPENAI_API_KEY") || strings.Contains(string(mcpBytes), "ANTHROPIC_API_KEY") {
				t.Fatal("plugin MCP configuration copied provider credentials")
			}
		})
	}
}

func TestFileInstallerRefusesForeignMarketplaceAndUnownedCache(t *testing.T) {
	t.Parallel()
	installation, root := newInstallerInstallation(t, HarnessCodex)
	dataRoot := filepath.Join(root, "data")
	installer, err := NewFileInstaller(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := installer.paths(HarnessCodex, installation)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureDataRoot(dataRoot); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(dataRoot, "personastack"), filepath.Join(dataRoot, "personastack", "LocalHarnessPlugins"), filepath.Join(dataRoot, "personastack", "LocalHarnessPlugins", paths.harness), paths.root} {
		if err := makePrivateDirectories(directory); err != nil {
			t.Fatal(err)
		}
	}
	runner := &pluginRunner{harness: HarnessCodex, profile: installation.Profile, marketplaceName: paths.marketplaceName, foreignMarketplace: "/tmp/foreign-marketplace"}
	installer.run = runner.run
	if err := installer.Preflight(context.Background(), HarnessCodex, installation); !errors.Is(err, ErrUnsafeFiles) {
		t.Fatalf("Preflight() foreign marketplace error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("foreign marketplace preflight made %d calls", len(runner.calls))
	}
	if err := hardenPluginCache(filepath.Join(installation.Profile, "plugins", "cache", "market", "plugin", "1"), root, installation.Profile, harnessPluginOwnership{Marketplace: "market", Plugin: "plugin"}); !errors.Is(err, ErrUnsafeFiles) {
		t.Fatalf("unowned cache error = %v", err)
	}
}

func TestFileInstallerRejectsDuplicateCLIRegistryFields(t *testing.T) {
	t.Parallel()
	installation, root := newInstallerInstallation(t, HarnessCodex)
	installer, err := NewFileInstaller(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := installer.paths(HarnessCodex, installation)
	if err != nil {
		t.Fatal(err)
	}
	installer.run = func(_ context.Context, _ string, arguments, _ []string) ([]byte, error) {
		if arguments[1] == "marketplace" {
			return []byte(`{"marketplaces":[],"marketplaces":[]}`), nil
		}
		return []byte(`{"installed":[],"installed":[]}`), nil
	}
	if _, err := installer.marketplaceRegistration(context.Background(), HarnessCodex, installation.Executable, nil, paths.marketplaceName, nil); !errors.Is(err, ErrUnsafeFiles) {
		t.Fatalf("duplicate marketplace fields error = %v", err)
	}
	if _, err := installer.verifyInstalled(context.Background(), HarnessCodex, installation, nil, paths, harnessPluginOwnership{
		Plugin: paths.pluginName, Marketplace: paths.marketplaceName,
	}, false); !errors.Is(err, ErrUnsafeFiles) {
		t.Fatalf("duplicate plugin fields error = %v", err)
	}
}

func TestRenameSkillPreservesMetadataAndRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()
	content := "---\nname: original\ndescription: >-\n  Keep the original summary.\nlicense: MIT\nmetadata:\n  targets:\n    - codex\n---\nBody stays here.\n"
	rename, err := renameSkill(content, "ps-0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("renameSkill() error = %v", err)
	}
	if !strings.Contains(rename, "name: ps-0123456789abcdef0123456789abcdef01234567") ||
		!strings.Contains(rename, "license: MIT") || !strings.Contains(rename, "targets:") || !strings.HasSuffix(rename, "Body stays here.\n") {
		t.Fatalf("renamed skill did not preserve metadata and body:\n%s", rename)
	}
	duplicate := "---\nname: one\nname: two\ndescription: summary\n---\n"
	if _, err := renameSkill(duplicate, "ps-0123456789abcdef0123456789abcdef01234567"); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("duplicate skill keys error = %v", err)
	}
}

func TestFileInstallerReplacesOnlyItsOwnedPluginSource(t *testing.T) {
	t.Parallel()
	installation, root := newInstallerInstallation(t, HarnessCodex)
	profileConfig := filepath.Join(installation.Profile, "config.toml")
	if err := os.WriteFile(profileConfig, []byte("existing user config"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(root, "xdg", "share")
	installer, err := NewFileInstaller(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	runner := &pluginRunner{harness: HarnessCodex, profile: installation.Profile, marketplaceName: installerMarketName(t, installer, HarnessCodex, installation)}
	installer.run = runner.run
	now := time.Now().UTC().Truncate(time.Second)
	bundle := validBundle(now)
	firstID := "c725451f-2d11-4e46-adfd-e92f2fc84c01"
	if err := installer.Configure(context.Background(), bundle, "https://my.personastack.ai", firstID, installation); err != nil {
		t.Fatalf("first Configure() error = %v", err)
	}
	paths, err := installer.paths(HarnessCodex, installation)
	if err != nil {
		t.Fatal(err)
	}
	first, err := readActive(paths)
	if err != nil || first == nil {
		t.Fatalf("first active ownership = %#v, %v", first, err)
	}
	secondID := "ecb2aee9-f0f0-42e7-aa9d-d3c129e81d97"
	if err := installer.Configure(context.Background(), bundle, "https://my.personastack.ai", secondID, installation); err != nil {
		t.Fatalf("replacement Configure() error = %v", err)
	}
	active, err := readActive(paths)
	if err != nil || active == nil || active.Source == first.Source {
		t.Fatalf("replacement active ownership = %#v, %v", active, err)
	}
	if _, err := os.Lstat(first.Source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old PersonaStack source was not removed: %v", err)
	}
	if runner.installed == false {
		t.Fatal("replacement plugin is not installed")
	}
	config, err := os.ReadFile(profileConfig)
	if err != nil || string(config) != "existing user config" {
		t.Fatalf("existing CLI config changed: %q, %v", config, err)
	}
}

func installerMarketName(t *testing.T, installer *FileInstaller, harness string, installation HarnessInstallation) string {
	t.Helper()
	paths, err := installer.paths(harness, installation)
	if err != nil {
		t.Fatal(err)
	}
	return paths.marketplaceName
}

func newInstallerInstallation(t *testing.T, harness string) (HarnessInstallation, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	profileName := ".codex"
	binary := "codex"
	if harness == HarnessClaudeCode {
		profileName = ".claude"
		binary = "claude"
	}
	profile := filepath.Join(home, profileName)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "bin", binary)
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return HarnessInstallation{
		Executable: executable, Home: home, Profile: profile, Shell: "/bin/bash",
		Environment: []string{"HOME=" + home, "PATH=/bin", "OPENAI_API_KEY=must-not-be-written"},
	}, root
}

func assertPrivateTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("unexpected symlink %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type pluginRunner struct {
	harness            string
	profile            string
	marketplaceName    string
	marketplacePath    string
	foreignMarketplace string
	installed          bool
	calls              [][]string
}

func (r *pluginRunner) run(_ context.Context, executable string, arguments, _ []string) ([]byte, error) {
	if executable == "" || len(arguments) < 3 || arguments[0] != "plugin" {
		return nil, ErrUnsafeFiles
	}
	r.calls = append(r.calls, append([]string(nil), arguments...))
	if arguments[1] == "marketplace" && arguments[2] == "list" {
		return r.marketplaceList()
	}
	if arguments[1] == "marketplace" && arguments[2] == "add" && len(arguments) == 4 {
		r.marketplacePath = arguments[3]
		return nil, nil
	}
	if arguments[1] == "marketplace" && arguments[2] == "remove" {
		r.marketplacePath = ""
		r.installed = false
		return nil, nil
	}
	if arguments[1] == "remove" || arguments[1] == "uninstall" {
		r.installed = false
		return nil, nil
	}
	if (arguments[1] == "add" || arguments[1] == "install") && r.marketplacePath != "" {
		if err := r.installCache(); err != nil {
			return nil, err
		}
		r.installed = true
		return nil, nil
	}
	if arguments[1] == "list" && len(arguments) >= 4 && arguments[2] == "--available" {
		return r.pluginList()
	}
	return nil, ErrUnsafeFiles
}

func (r *pluginRunner) marketplaceList() ([]byte, error) {
	registeredPath := r.marketplacePath
	if r.foreignMarketplace != "" {
		registeredPath = r.foreignMarketplace
	}
	if r.harness == HarnessCodex {
		entries := []struct {
			Name string `json:"name"`
			Root string `json:"root"`
		}{}
		if registeredPath != "" {
			entries = append(entries, struct {
				Name string `json:"name"`
				Root string `json:"root"`
			}{Name: r.marketplaceName, Root: registeredPath})
		}
		return json.Marshal(struct {
			Marketplaces []struct {
				Name string `json:"name"`
				Root string `json:"root"`
			} `json:"marketplaces"`
		}{Marketplaces: entries})
	}
	entries := []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}{}
	if registeredPath != "" {
		entries = append(entries, struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}{Name: r.marketplaceName, Path: registeredPath})
	}
	return json.Marshal(entries)
}

func (r *pluginRunner) installCache() error {
	marketplace := filepath.Join(r.marketplacePath, "marketplace.json")
	if r.harness == HarnessCodex {
		marketplace = filepath.Join(r.marketplacePath, ".agents", "plugins", "marketplace.json")
	} else {
		marketplace = filepath.Join(r.marketplacePath, ".claude-plugin", "marketplace.json")
	}
	data, err := os.ReadFile(marketplace)
	if err != nil {
		return err
	}
	var manifest struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"plugins"`
	}
	if r.harness == HarnessClaudeCode {
		var claude struct {
			Name    string `json:"name"`
			Plugins []struct {
				Name   string `json:"name"`
				Source string `json:"source"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(data, &claude); err != nil || len(claude.Plugins) != 1 {
			return ErrUnsafeFiles
		}
		manifest.Name = claude.Name
		manifest.Plugins = []struct {
			Name   string `json:"name"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		}{{Name: claude.Plugins[0].Name}}
		manifest.Plugins[0].Source.Path = claude.Plugins[0].Source
	} else if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Plugins) != 1 {
		return ErrUnsafeFiles
	}
	plugin := filepath.Join(r.marketplacePath, filepath.FromSlash(manifest.Plugins[0].Source.Path))
	cache := filepath.Join(r.profile, "plugins", "cache", manifest.Name, manifest.Plugins[0].Name, "1.0.0")
	if err := copyTree(plugin, cache); err != nil {
		return err
	}
	return nil
}

func (r *pluginRunner) pluginList() ([]byte, error) {
	if !r.installed {
		return json.Marshal(struct {
			Installed []any `json:"installed"`
		}{Installed: []any{}})
	}
	marketplace := filepath.Join(r.marketplacePath, "marketplace.json")
	if r.harness == HarnessCodex {
		marketplace = filepath.Join(r.marketplacePath, ".agents", "plugins", "marketplace.json")
	} else {
		marketplace = filepath.Join(r.marketplacePath, ".claude-plugin", "marketplace.json")
	}
	data, err := os.ReadFile(marketplace)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Plugins) != 1 {
		return nil, ErrUnsafeFiles
	}
	identifier := manifest.Plugins[0].Name + "@" + manifest.Name
	cache := filepath.Join(r.profile, "plugins", "cache", manifest.Name, manifest.Plugins[0].Name, "1.0.0")
	if r.harness == HarnessCodex {
		return json.Marshal(struct {
			Installed []any `json:"installed"`
		}{Installed: []any{map[string]any{
			"pluginId": identifier, "enabled": true, "version": "1.0.0",
			"source":            map[string]string{"path": filepath.Join(r.marketplacePath, "plugins", manifest.Plugins[0].Name)},
			"marketplaceSource": map[string]string{"source": r.marketplacePath},
		}}})
	}
	return json.Marshal(struct {
		Installed []any `json:"installed"`
	}{Installed: []any{map[string]any{"id": identifier, "enabled": true, "scope": "user", "installPath": cache}}})
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
