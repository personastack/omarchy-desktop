//go:build linux

package localsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
	"gopkg.in/yaml.v3"
)

const pluginCommandTimeout = 30 * time.Second

var fileInstallerConfigureMu sync.Mutex

type pluginCommandRunner func(context.Context, string, []string, []string) ([]byte, error)

type FileInstaller struct {
	dataRoot string
	run      pluginCommandRunner
}

type harnessPluginOwnership struct {
	Format      int    `json:"format"`
	Harness     string `json:"harness"`
	Marketplace string `json:"marketplace"`
	Plugin      string `json:"plugin"`
	Profile     string `json:"profile"`
	Source      string `json:"source"`
	Digest      string `json:"digest"`
}

type localSkillOwnership struct {
	Digest    string `json:"digest"`
	Format    int    `json:"format"`
	Origin    string `json:"origin"`
	Persona   string `json:"persona"`
	Profile   string `json:"profile"`
	Skill     string `json:"skill"`
	Workspace string `json:"workspace"`
}

type pluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Skills      string `json:"skills,omitempty"`
	MCPServers  string `json:"mcpServers,omitempty"`
}

type mcpHeaders struct {
	Authorization string `json:"Authorization"`
}

type mcpServer struct {
	Type    string     `json:"type"`
	URL     string     `json:"url"`
	Headers mcpHeaders `json:"headers"`
}

type codexMCPConfig struct {
	Servers map[string]mcpServer `json:"mcpServers"`
}

type claudeMCPConfig struct {
	PersonaStackLocal mcpServer `json:"personastack_local"`
}

type codexMarketplace struct {
	Name    string              `json:"name"`
	Plugins []marketplacePlugin `json:"plugins"`
}

type marketplacePlugin struct {
	Name   string       `json:"name"`
	Source pluginSource `json:"source"`
}

type pluginSource struct {
	Source string `json:"source"`
	Path   string `json:"path"`
}

type claudeMarketplace struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Owner       marketplaceOwner          `json:"owner"`
	Plugins     []claudeMarketplacePlugin `json:"plugins"`
}

type marketplaceOwner struct {
	Name string `json:"name"`
}

type claudeMarketplacePlugin struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

type codexMarketplaceList struct {
	Marketplaces *[]struct {
		Name string `json:"name"`
		Root string `json:"root"`
	} `json:"marketplaces"`
}

type claudeMarketplaceList []struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type installedPlugin struct {
	PluginID    string `json:"pluginId"`
	ID          string `json:"id"`
	Enabled     bool   `json:"enabled"`
	Scope       string `json:"scope"`
	Version     string `json:"version"`
	InstallPath string `json:"installPath"`
	Source      struct {
		Path string `json:"path"`
	} `json:"source"`
	MarketplaceSource struct {
		Source string `json:"source"`
	} `json:"marketplaceSource"`
}

type pluginList struct {
	Installed *[]installedPlugin `json:"installed"`
}

func NewFileInstaller(dataRoot string) (*FileInstaller, error) {
	if !filepath.IsAbs(dataRoot) {
		return nil, ErrInvalidRequest
	}
	return &FileInstaller{dataRoot: filepath.Clean(dataRoot), run: runPluginCommand}, nil
}

func DefaultFileInstaller() (*FileInstaller, error) {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, ErrUnavailable
		}
		root = filepath.Join(userHome, ".local", "share")
	}
	return NewFileInstaller(root)
}

func (f *FileInstaller) Preflight(ctx context.Context, harness string, installation HarnessInstallation) error {
	if f == nil || f.run == nil || !validHarness(harness) || validateInstallationPaths(installation, harness) != nil {
		return ErrUnsafeFiles
	}
	paths, err := f.paths(harness, installation)
	if err != nil {
		return ErrUnsafeFiles
	}
	if err := validateExistingTree(paths.root, true); err != nil {
		return ErrUnsafeFiles
	}
	active, err := readActive(paths)
	if err != nil {
		return ErrUnsafeFiles
	}
	environment := pluginEnvironment(harness, installation)
	registration, err := f.marketplaceRegistration(ctx, harness, installation.Executable, environment, paths.marketplaceName, active)
	if err != nil {
		return ErrUnsafeFiles
	}
	if active == nil && registration != marketplaceMissing || active != nil && registration == marketplaceMismatch {
		return ErrUnsafeFiles
	}
	return nil
}

func (f *FileInstaller) Configure(ctx context.Context, bundle apicontract.LocalSessionResponse, origin, sessionID string, installation HarnessInstallation) error {
	fileInstallerConfigureMu.Lock()
	defer fileInstallerConfigureMu.Unlock()
	if f == nil || f.run == nil || ValidateBundle(bundle, origin, time.Now()) != nil || !validHarness(string(bundle.Harness)) || validateInstallationPaths(installation, string(bundle.Harness)) != nil {
		return ErrUnsafeFiles
	}
	paths, err := f.paths(string(bundle.Harness), installation)
	if err != nil || !uuidPattern.MatchString(sessionID) {
		return ErrUnsafeFiles
	}
	if err := validateExistingTree(paths.root, true); err != nil {
		return ErrUnsafeFiles
	}
	if err := ensureDataRoot(f.dataRoot); err != nil {
		return ErrUnsafeFiles
	}
	previous, err := readActive(paths)
	if err != nil {
		return ErrUnsafeFiles
	}
	environment := pluginEnvironment(string(bundle.Harness), installation)
	registration, err := f.marketplaceRegistration(ctx, string(bundle.Harness), installation.Executable, environment, paths.marketplaceName, previous)
	if err != nil || previous == nil && registration != marketplaceMissing || previous != nil && registration == marketplaceMismatch {
		return ErrUnsafeFiles
	}
	directory := filepath.Join(paths.root, sessionID)
	if _, err := os.Lstat(directory); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeFiles
	}
	for _, parent := range []string{filepath.Join(f.dataRoot, "personastack"), filepath.Join(f.dataRoot, "personastack", "LocalHarnessPlugins"), filepath.Join(f.dataRoot, "personastack", "LocalHarnessPlugins", paths.harness), paths.root} {
		if err := makePrivateDirectories(parent); err != nil {
			return ErrUnsafeFiles
		}
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return ErrUnsafeFiles
	}
	registered := false
	completed := false
	staged := harnessPluginOwnership{}
	defer func() {
		if registered && !completed {
			_ = writeActive(paths, staged)
		} else if !registered {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := f.writeSource(directory, paths, bundle, origin); err != nil {
		return ErrUnsafeFiles
	}
	staged, err = makeOwnership(paths, directory)
	if err != nil || writeJSON(filepath.Join(directory, ".personastack-plugin-owner.json"), staged) != nil {
		return ErrUnsafeFiles
	}
	if previous != nil {
		if registration == marketplaceMatches {
			installed, verifyErr := f.verifyInstalled(ctx, string(bundle.Harness), installation, environment, paths, *previous, true)
			if verifyErr != nil {
				return ErrUnsafeFiles
			}
			if installed {
				remove := []string{"plugin", "remove", previous.Plugin + "@" + previous.Marketplace}
				if bundle.Harness == apicontract.LocalSessionHarnessClaudeCode {
					remove = []string{"plugin", "uninstall", previous.Plugin + "@" + previous.Marketplace, "--scope", "user"}
				}
				if _, err := f.run(ctx, installation.Executable, remove, environment); err != nil {
					return ErrUnsafeFiles
				}
			}
			if _, err := f.run(ctx, installation.Executable, []string{"plugin", "marketplace", "remove", previous.Marketplace}, environment); err != nil {
				return ErrUnsafeFiles
			}
		}
	}
	if _, err := f.run(ctx, installation.Executable, []string{"plugin", "marketplace", "add", filepath.Join(directory, "marketplace")}, environment); err != nil {
		return ErrUnsafeFiles
	}
	registered = true
	install := []string{"plugin", "add", paths.pluginName, "--marketplace", paths.marketplaceName}
	if bundle.Harness == apicontract.LocalSessionHarnessClaudeCode {
		install = []string{"plugin", "install", paths.pluginName + "@" + paths.marketplaceName, "--scope", "user"}
	}
	if _, err := f.run(ctx, installation.Executable, install, environment); err != nil {
		return ErrUnsafeFiles
	}
	if _, err := f.verifyInstalled(ctx, string(bundle.Harness), installation, environment, paths, staged, false); err != nil {
		return ErrUnsafeFiles
	}
	if err := writeActive(paths, staged); err != nil {
		return ErrUnsafeFiles
	}
	if previous != nil && previous.Source != directory {
		if err := verifyOwnedSource(*previous, paths); err != nil {
			return ErrUnsafeFiles
		}
		_ = os.RemoveAll(previous.Source)
	}
	completed = true
	return nil
}

type installerPaths struct {
	root            string
	profile         string
	marketplaceName string
	pluginName      string
	harness         string
}

func (f *FileInstaller) paths(harness string, installation HarnessInstallation) (installerPaths, error) {
	profile, err := filepath.EvalSymlinks(installation.Profile)
	if err != nil {
		return installerPaths{}, err
	}
	profile, err = filepath.Abs(profile)
	if err != nil {
		return installerPaths{}, err
	}
	profileDigest := sha256.Sum256([]byte(profile))
	profileKey := hex.EncodeToString(profileDigest[:16])
	profileName := "codex"
	if harness == HarnessClaudeCode {
		profileName = "claude-code"
	}
	root := filepath.Join(f.dataRoot, "personastack", "LocalHarnessPlugins", profileName, profileKey)
	return installerPaths{
		root: root, profile: profile, marketplaceName: "personastack-desktop-" + profileKey,
		pluginName: "personastack-local-" + profileKey, harness: profileName,
	}, nil
}

func (f *FileInstaller) writeSource(directory string, paths installerPaths, bundle apicontract.LocalSessionResponse, origin string) error {
	marketplace := filepath.Join(directory, "marketplace")
	plugin := filepath.Join(marketplace, "plugins", paths.pluginName)
	metadataDirectory := ".codex-plugin"
	if bundle.Harness == apicontract.LocalSessionHarnessClaudeCode {
		metadataDirectory = ".claude-plugin"
	}
	pluginMetadata := filepath.Join(plugin, metadataDirectory)
	if err := makePrivateDirectories(paths.root, directory, marketplace, filepath.Join(marketplace, "plugins"), plugin, pluginMetadata); err != nil {
		return err
	}
	manifest := pluginManifest{
		Name: paths.pluginName, Version: "1.0.0", Description: "Active PersonaStack local persona context and MCP connection.",
	}
	if bundle.Harness == apicontract.LocalSessionHarnessCodex {
		manifest.Skills = "./skills/"
		manifest.MCPServers = "./.mcp.json"
	}
	if err := writeJSON(filepath.Join(pluginMetadata, "plugin.json"), manifest); err != nil {
		return err
	}
	server := mcpServer{Type: "http", URL: bundle.MCPURL, Headers: mcpHeaders{Authorization: "Bearer " + bundle.BearerToken}}
	if bundle.Harness == apicontract.LocalSessionHarnessCodex {
		if err := writeJSON(filepath.Join(plugin, ".mcp.json"), codexMCPConfig{Servers: map[string]mcpServer{"personastack_local": server}}); err != nil {
			return err
		}
	} else if err := writeJSON(filepath.Join(plugin, ".mcp.json"), claudeMCPConfig{PersonaStackLocal: server}); err != nil {
		return err
	}
	baseSkill := "---\nname: personastack\ndescription: Active PersonaStack persona context. Consult this skill before work involving the configured persona and use the PersonaStack MCP server for current persona state.\n---\n\n" + bundle.PersonaPrompt + "\n"
	if err := writeArtifact(plugin, "skills/personastack/SKILL.md", baseSkill); err != nil {
		return err
	}
	for _, skill := range bundle.Skills {
		name, ownership, files, err := prepareSkill(skill, bundle, origin, paths.profile)
		if err != nil {
			return err
		}
		for _, file := range files {
			if strings.EqualFold(file.RelativePath, ".personastack-owner.json") {
				return ErrUnsafeFiles
			}
			if err := writeArtifact(plugin, filepath.Join("skills", name, filepath.FromSlash(file.RelativePath)), file.Content); err != nil {
				return err
			}
		}
		if err := writeJSON(filepath.Join(plugin, "skills", name, ".personastack-owner.json"), ownership); err != nil {
			return err
		}
	}
	if bundle.Harness == apicontract.LocalSessionHarnessCodex {
		if err := makePrivateDirectories(filepath.Join(marketplace, ".agents"), filepath.Join(marketplace, ".agents", "plugins")); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(marketplace, ".agents", "plugins", "marketplace.json"), codexMarketplace{
			Name:    paths.marketplaceName,
			Plugins: []marketplacePlugin{{Name: paths.pluginName, Source: pluginSource{Source: "local", Path: "./plugins/" + paths.pluginName}}},
		}); err != nil {
			return err
		}
	} else {
		if err := makePrivateDirectories(filepath.Join(marketplace, ".claude-plugin")); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(marketplace, ".claude-plugin", "marketplace.json"), claudeMarketplace{
			Name: paths.marketplaceName, Description: "Desktop-managed PersonaStack local harness plugin.",
			Owner:   marketplaceOwner{Name: "PersonaStack"},
			Plugins: []claudeMarketplacePlugin{{Name: paths.pluginName, Source: "./plugins/" + paths.pluginName, Version: "1.0.0"}},
		}); err != nil {
			return err
		}
	}
	return nil
}

type preparedSkillFile struct {
	RelativePath string
	Content      string
}

func prepareSkill(skill apicontract.LocalSessionSkill, bundle apicontract.LocalSessionResponse, origin, profile string) (string, localSkillOwnership, []preparedSkillFile, error) {
	ownership := localSkillOwnership{
		Format: 1, Origin: origin, Workspace: bundle.WorkspaceID, Persona: bundle.PersonaID,
		Profile: profile, Skill: skill.SkillID, Digest: skill.Digest,
	}
	data, err := json.Marshal(ownership)
	if err != nil {
		return "", localSkillOwnership{}, nil, ErrInvalidBundle
	}
	identity := sha256.Sum256(data)
	name := "ps-" + hex.EncodeToString(identity[:20])
	files := make([]preparedSkillFile, 0, len(skill.Files))
	for _, file := range skill.Files {
		content := file.Content
		if file.RelativePath == "SKILL.md" {
			content, err = renameSkill(content, name)
			if err != nil {
				return "", localSkillOwnership{}, nil, err
			}
		}
		files = append(files, preparedSkillFile{RelativePath: file.RelativePath, Content: content})
	}
	return name, ownership, files, nil
}

func renameSkill(content, name string) (string, error) {
	if !strings.HasPrefix(name, "ps-") || len(content) > maxSessionFileBytes {
		return "", ErrInvalidBundle
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 4 || strings.TrimSpace(lines[0]) != "---" {
		return "", ErrInvalidBundle
	}
	end := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			end = index
			break
		}
	}
	if end < 0 {
		return "", ErrInvalidBundle
	}
	var document yaml.Node
	frontmatter := strings.Join(lines[1:end], "\n")
	if err := yaml.Unmarshal([]byte(frontmatter), &document); err != nil || len(document.Content) != 1 {
		return "", ErrInvalidBundle
	}
	mapping := document.Content[0]
	if mapping.Kind != yaml.MappingNode || len(mapping.Content)%2 != 0 {
		return "", ErrInvalidBundle
	}
	seen := make(map[string]struct{}, len(mapping.Content)/2)
	nameValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}
	nameFound := false
	descriptionFound := false
	for index := 0; index < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		value := mapping.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return "", ErrInvalidBundle
		}
		if _, exists := seen[key.Value]; exists {
			return "", ErrInvalidBundle
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "name":
			mapping.Content[index+1] = nameValue
			nameFound = true
		case "description":
			descriptionFound = value.Kind == yaml.ScalarNode && value.Value != ""
		}
	}
	if !descriptionFound {
		return "", ErrInvalidBundle
	}
	if !nameFound {
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"}, nameValue)
	}
	encoded, err := yaml.Marshal(mapping)
	if err != nil {
		return "", ErrInvalidBundle
	}
	body := strings.Join(lines[end+1:], "\n")
	return "---\n" + strings.TrimSuffix(string(encoded), "\n") + "\n---\n" + body, nil
}

func makeOwnership(paths installerPaths, directory string) (harnessPluginOwnership, error) {
	digest, err := contentDigest(directory, true, true)
	if err != nil {
		return harnessPluginOwnership{}, err
	}
	return harnessPluginOwnership{
		Format: 1, Harness: paths.harness, Marketplace: paths.marketplaceName, Plugin: paths.pluginName,
		Profile: paths.profile, Source: directory, Digest: digest,
	}, nil
}

func readActive(paths installerPaths) (*harnessPluginOwnership, error) {
	activePath := filepath.Join(paths.root, "active.json")
	file, err := os.OpenFile(activePath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if verifyPrivateFile(file) != nil {
		return nil, ErrUnsafeFiles
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 || !wirejson.ValidUniqueJSON(data) {
		return nil, ErrUnsafeFiles
	}
	var ownership harnessPluginOwnership
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ownership); err != nil || decoder.Decode(new(any)) != io.EOF ||
		ownership.Format != 1 || ownership.Harness != paths.harness || ownership.Marketplace != paths.marketplaceName || ownership.Plugin != paths.pluginName || ownership.Profile != paths.profile {
		return nil, ErrUnsafeFiles
	}
	if err := verifyOwnedSource(ownership, paths); err != nil {
		return nil, err
	}
	return &ownership, nil
}

func verifyOwnedSource(ownership harnessPluginOwnership, paths installerPaths) error {
	source, err := filepath.Abs(ownership.Source)
	if err != nil || filepath.Clean(source) != filepath.Clean(ownership.Source) || filepath.Dir(source) != paths.root {
		return ErrUnsafeFiles
	}
	if err := verifyPrivateDirectory(source); err != nil {
		return ErrUnsafeFiles
	}
	markerPath := filepath.Join(source, ".personastack-plugin-owner.json")
	marker, err := os.OpenFile(markerPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrUnsafeFiles
	}
	defer marker.Close()
	if verifyPrivateFile(marker) != nil {
		return ErrUnsafeFiles
	}
	data, err := io.ReadAll(io.LimitReader(marker, 4097))
	if err != nil || len(data) > 4096 || !wirejson.ValidUniqueJSON(data) {
		return ErrUnsafeFiles
	}
	var recorded harnessPluginOwnership
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recorded); err != nil || decoder.Decode(new(any)) != io.EOF || recorded != ownership {
		return ErrUnsafeFiles
	}
	digest, err := contentDigest(source, true, true)
	if err != nil || digest != ownership.Digest {
		return ErrUnsafeFiles
	}
	return nil
}

func contentDigest(root string, requirePrivate bool, excludeOwner bool) (string, error) {
	type fileRecord struct {
		path string
		data []byte
	}
	files := make([]fileRecord, 0)
	if requirePrivate && verifyPrivateDirectory(root) != nil {
		return "", ErrUnsafeFiles
	}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrUnsafeFiles
		}
		info, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeFiles
		}
		if entry.IsDir() {
			if requirePrivate && current != root && verifyPrivateDirectory(current) != nil {
				return ErrUnsafeFiles
			}
			return nil
		}
		if !info.Mode().IsRegular() || requirePrivate && verifyPrivateFileInfo(info) != nil {
			return ErrUnsafeFiles
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return ErrUnsafeFiles
		}
		if excludeOwner && relative == ".personastack-plugin-owner.json" {
			return nil
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return ErrUnsafeFiles
		}
		files = append(files, fileRecord{path: filepath.ToSlash(relative), data: data})
		return nil
	})
	if err != nil {
		return "", ErrUnsafeFiles
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	hash := sha256.New()
	for _, file := range files {
		_, _ = hash.Write([]byte(file.path))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(file.data)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type marketplaceState int

const (
	marketplaceMissing marketplaceState = iota
	marketplaceMatches
	marketplaceMismatch
)

func (f *FileInstaller) marketplaceRegistration(ctx context.Context, harness, executable string, environment []string, name string, active *harnessPluginOwnership) (marketplaceState, error) {
	output, err := f.run(ctx, executable, []string{"plugin", "marketplace", "list", "--json"}, environment)
	if err != nil {
		return marketplaceMismatch, err
	}
	if !wirejson.ValidUniqueJSON(output) {
		return marketplaceMismatch, ErrUnsafeFiles
	}
	var registeredPath string
	found := false
	if harness == HarnessCodex {
		var response codexMarketplaceList
		if json.Unmarshal(output, &response) != nil || response.Marketplaces == nil {
			return marketplaceMismatch, ErrUnsafeFiles
		}
		for _, entry := range *response.Marketplaces {
			if entry.Name == name {
				if found {
					return marketplaceMismatch, ErrUnsafeFiles
				}
				found = true
				registeredPath = entry.Root
			}
		}
	} else {
		var response claudeMarketplaceList
		if !json.Valid(output) || len(bytes.TrimSpace(output)) == 0 || bytes.TrimSpace(output)[0] != '[' || json.Unmarshal(output, &response) != nil {
			return marketplaceMismatch, ErrUnsafeFiles
		}
		for _, entry := range response {
			if entry.Name == name {
				if found {
					return marketplaceMismatch, ErrUnsafeFiles
				}
				found = true
				registeredPath = entry.Path
			}
		}
	}
	if registeredPath == "" {
		return marketplaceMissing, nil
	}
	if active == nil {
		return marketplaceMismatch, nil
	}
	expected := filepath.Join(active.Source, "marketplace")
	if sameDirectory(registeredPath, expected) {
		return marketplaceMatches, nil
	}
	return marketplaceMismatch, nil
}

func (f *FileInstaller) verifyInstalled(ctx context.Context, harness string, installation HarnessInstallation, environment []string, paths installerPaths, ownership harnessPluginOwnership, allowMissing bool) (bool, error) {
	output, err := f.run(ctx, installation.Executable, []string{"plugin", "list", "--available", "--json"}, environment)
	if err != nil {
		return false, err
	}
	if !wirejson.ValidUniqueJSON(output) {
		return false, ErrUnsafeFiles
	}
	var response pluginList
	if json.Unmarshal(output, &response) != nil || response.Installed == nil {
		return false, ErrUnsafeFiles
	}
	identifier := ownership.Plugin + "@" + ownership.Marketplace
	matchCount := 0
	var matched installedPlugin
	for _, item := range *response.Installed {
		matchedID := item.ID
		if harness == HarnessCodex {
			matchedID = item.PluginID
		}
		if matchedID == identifier {
			matchCount++
			matched = item
		}
	}
	if matchCount > 1 {
		return false, ErrUnsafeFiles
	}
	if matchCount == 1 {
		item := matched
		if harness == HarnessCodex && item.PluginID == identifier {
			if !item.Enabled || !sameDirectory(item.Source.Path, filepath.Join(ownership.Source, "marketplace", "plugins", ownership.Plugin)) ||
				!sameDirectory(item.MarketplaceSource.Source, filepath.Join(ownership.Source, "marketplace")) || item.Version == "" {
				return false, ErrUnsafeFiles
			}
			cache := filepath.Join(paths.profile, "plugins", "cache", ownership.Marketplace, ownership.Plugin, item.Version)
			return true, hardenPluginCache(cache, filepath.Join(ownership.Source, "marketplace", "plugins", ownership.Plugin), paths.profile, ownership)
		}
		if harness == HarnessClaudeCode && item.ID == identifier {
			if !item.Enabled || item.Scope != "user" || item.InstallPath == "" {
				return false, ErrUnsafeFiles
			}
			return true, hardenPluginCache(item.InstallPath, filepath.Join(ownership.Source, "marketplace", "plugins", ownership.Plugin), paths.profile, ownership)
		}
	}
	if allowMissing {
		return false, nil
	}
	return false, ErrUnsafeFiles
}

func hardenPluginCache(cache, source, profile string, ownership harnessPluginOwnership) error {
	cachePath, err := filepath.Abs(cache)
	if err != nil || filepath.Clean(cachePath) != filepath.Clean(cache) {
		return ErrUnsafeFiles
	}
	cacheRoot := filepath.Join(profile, "plugins", "cache", ownership.Marketplace, ownership.Plugin)
	relative, err := filepath.Rel(cacheRoot, cachePath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrUnsafeFiles
	}
	resolved, err := filepath.EvalSymlinks(cachePath)
	if err != nil || resolved != cachePath {
		return ErrUnsafeFiles
	}
	expected, err := contentDigest(source, true, false)
	if err != nil {
		return err
	}
	observed, err := contentDigest(cachePath, false, false)
	if err != nil || observed != expected {
		return ErrUnsafeFiles
	}
	if err := filepath.WalkDir(cachePath, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
			return ErrUnsafeFiles
		}
		mode := os.FileMode(0o600)
		if entry.IsDir() {
			mode = 0o700
		}
		if err := os.Chmod(current, mode); err != nil {
			return ErrUnsafeFiles
		}
		return nil
	}); err != nil {
		return ErrUnsafeFiles
	}
	_, err = contentDigest(cachePath, true, false)
	return err
}

func writeActive(paths installerPaths, ownership harnessPluginOwnership) error {
	active := filepath.Join(paths.root, "active.json")
	if existing, err := os.Lstat(active); err == nil {
		if err := verifyPrivateFileInfo(existing); err != nil {
			return ErrUnsafeFiles
		}
		if err := os.Remove(active); err != nil {
			return ErrUnsafeFiles
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeFiles
	}
	return writeJSON(active, ownership)
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > 256*1024 {
		return ErrUnsafeFiles
	}
	return writePrivateFile(path, data)
}

func writeArtifact(root, relative, content string) error {
	if !safeRelativePath(filepath.ToSlash(relative)) {
		return ErrUnsafeFiles
	}
	components := strings.Split(filepath.ToSlash(relative), "/")
	directory := root
	for _, component := range components[:len(components)-1] {
		directory = filepath.Join(directory, component)
		if err := makePrivateDirectories(directory); err != nil {
			return err
		}
	}
	return writePrivateFile(filepath.Join(directory, components[len(components)-1]), []byte(content))
}

func makePrivateDirectories(paths ...string) error {
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err == nil {
			continue
		} else if !errors.Is(err, os.ErrExist) {
			return ErrUnsafeFiles
		}
		if verifyPrivateDirectory(path) != nil {
			return ErrUnsafeFiles
		}
	}
	return nil
}

func ensureDataRoot(root string) error {
	if !filepath.IsAbs(root) {
		return ErrUnsafeFiles
	}
	root = filepath.Clean(root)
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		if err := os.Mkdir(current, 0o700); err == nil {
			continue
		} else if !errors.Is(err, os.ErrExist) {
			return ErrUnsafeFiles
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeFiles
		}
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ErrUnsafeFiles
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) {
		return ErrUnsafeFiles
	}
	return nil
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrUnsafeFiles
	}
	if verifyPrivateFile(file) != nil {
		file.Close()
		return ErrUnsafeFiles
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ErrUnsafeFiles
	}
	return nil
}

func validateExistingTree(root string, allowMissing bool) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(root), string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if allowMissing {
				return nil
			}
			return ErrUnsafeFiles
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeFiles
		}
		if current == root && verifyPrivateDirectory(current) != nil {
			return ErrUnsafeFiles
		}
	}
	return nil
}

func validateInstallationPaths(installation HarnessInstallation, harness string) error {
	for _, value := range []string{installation.Executable, installation.Home, installation.Profile} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return ErrUnsafeFiles
		}
	}
	for _, value := range []string{installation.Home, installation.Profile} {
		resolved, err := filepath.EvalSymlinks(value)
		if err != nil {
			return ErrUnsafeFiles
		}
		info, err := os.Stat(resolved)
		if err != nil || info == nil || !info.IsDir() {
			return ErrUnsafeFiles
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) {
			return ErrUnsafeFiles
		}
	}
	info, err := os.Stat(installation.Executable)
	expectedBinary := "codex"
	if harness == HarnessClaudeCode {
		expectedBinary = "claude"
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || filepath.Base(installation.Executable) != expectedBinary {
		return ErrUnsafeFiles
	}
	return nil
}

func pluginEnvironment(harness string, installation HarnessInstallation) []string {
	environment := append([]string(nil), installation.Environment...)
	name := "CODEX_HOME"
	if harness == HarnessClaudeCode {
		name = "CLAUDE_CONFIG_DIR"
	}
	return setEnvironment(environment, map[string]string{"HOME": installation.Home, name: installation.Profile})
}

func setEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	seen := make(map[string]bool, len(replacements))
	for _, item := range environment {
		key, _, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		value, replace := replacements[key]
		if replace {
			result = append(result, key+"="+value)
			seen[key] = true
		} else {
			result = append(result, item)
		}
	}
	for key, value := range replacements {
		if !seen[key] {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func sameDirectory(actual, expected string) bool {
	if !filepath.IsAbs(actual) {
		return false
	}
	actualInfo, actualErr := os.Stat(actual)
	expectedInfo, expectedErr := os.Stat(expected)
	if actualErr != nil || expectedErr != nil || !actualInfo.IsDir() || !expectedInfo.IsDir() {
		return false
	}
	return os.SameFile(actualInfo, expectedInfo)
}

func verifyPrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return ErrUnsafeFiles
	}
	return verifyPrivateFileInfo(info)
}

func runPluginCommand(parent context.Context, executable string, arguments, environment []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, pluginCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = environment
	command.Stdin = strings.NewReader("")
	command.Stderr = io.Discard
	output := &limitedBuffer{limit: maxProbeBytes}
	command.Stdout = output
	if err := command.Run(); err != nil {
		return nil, ErrUnsafeFiles
	}
	if !json.Valid(output.Bytes()) && strings.Contains(strings.Join(arguments, " "), "--json") {
		return nil, ErrUnsafeFiles
	}
	return bytes.Clone(output.Bytes()), nil
}
