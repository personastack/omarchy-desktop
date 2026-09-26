package localsession

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

const (
	maxSessionSkills     = 32
	maxSessionFiles      = 128
	maxSessionFileBytes  = 512 * 1024
	maxSessionPromptSize = apicontract.LocalSessionMaxPromptBytes
	sessionLifetime      = 365 * 24 * time.Hour
)

var (
	workspaceIDPattern = regexp.MustCompile(`^ws_[0-9a-f]{32}$`)
	bearerPattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidateBundle enforces the producer-owned local-session wire contract
// before any file or CLI state is changed.
func ValidateBundle(bundle apicontract.LocalSessionResponse, appOrigin string, now time.Time) error {
	if err := validateIdentity(bundle); err != nil {
		return err
	}
	if err := validateLifetime(bundle, now); err != nil {
		return err
	}
	if !permitsMCP(bundle.MCPURL, appOrigin) {
		return ErrInvalidBundle
	}
	return validateSkills(bundle.Skills)
}

func validateIdentity(bundle apicontract.LocalSessionResponse) error {
	if !validHarness(string(bundle.Harness)) || !personaIDPattern.MatchString(bundle.PersonaID) ||
		!workspaceIDPattern.MatchString(bundle.WorkspaceID) || !utf8.ValidString(bundle.PersonaName) ||
		len([]byte(bundle.PersonaName)) > 1024 || strings.ContainsRune(bundle.PersonaName, '\x00') ||
		!utf8.ValidString(bundle.PersonaPrompt) || len([]byte(bundle.PersonaPrompt)) > maxSessionPromptSize ||
		strings.ContainsRune(bundle.PersonaPrompt, '\x00') || !bearerPattern.MatchString(bundle.BearerToken) {
		return ErrInvalidBundle
	}
	return nil
}

func validateLifetime(bundle apicontract.LocalSessionResponse, now time.Time) error {
	issued, err := time.Parse(time.RFC3339Nano, bundle.IssuedAt)
	if err != nil {
		return ErrInvalidBundle
	}
	expires, err := time.Parse(time.RFC3339Nano, bundle.ExpiresAt)
	if err != nil || expires.Sub(issued) != sessionLifetime || issued.After(now.Add(5*time.Minute)) || !expires.After(now) {
		return ErrInvalidBundle
	}
	return nil
}

func permitsMCP(endpoint, appOrigin string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v1/mcp" {
		return false
	}
	switch appOrigin {
	case "https://my.personastack.ai":
		return endpoint == "https://mcp.personastack.ai/v1/mcp"
	case "https://personastack.ericgreer.info":
		return endpoint == "http://mcp.personastack.lan/v1/mcp"
	default:
		return false
	}
}

func validateSkills(skills []apicontract.LocalSessionSkill) error {
	if len(skills) > maxSessionSkills {
		return ErrInvalidBundle
	}
	seenIDs := make(map[string]struct{}, len(skills))
	fileCount := 0
	totalBytes := 0
	for _, skill := range skills {
		if len(skill.SkillID) == 0 || len([]byte(skill.SkillID)) > 512 || strings.ContainsRune(skill.SkillID, '\x00') ||
			len(skill.Slug) == 0 || len([]byte(skill.Slug)) > 255 || strings.ContainsRune(skill.Slug, '\x00') ||
			!digestPattern.MatchString(skill.Digest) || len(skill.Files) == 0 || len(skill.Files) > maxSessionFiles {
			return ErrInvalidBundle
		}
		if _, exists := seenIDs[skill.SkillID]; exists {
			return ErrInvalidBundle
		}
		seenIDs[skill.SkillID] = struct{}{}
		if err := validateSkillFiles(skill.Files); err != nil {
			return err
		}
		if err := validateSkillDigest(skill); err != nil {
			return err
		}
		fileCount += len(skill.Files)
		for _, file := range skill.Files {
			totalBytes += len([]byte(file.Content))
		}
		if fileCount > maxSessionFiles || totalBytes > maxSessionFileBytes {
			return ErrInvalidBundle
		}
	}
	return nil
}

func validateSkillFiles(files []apicontract.LocalSessionSkillFile) error {
	seen := make(map[string]struct{}, len(files))
	hasSkill := false
	for _, file := range files {
		if !safeRelativePath(file.RelativePath) || !utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, '\x00') {
			return ErrInvalidBundle
		}
		key := strings.ToLower(file.RelativePath)
		if _, exists := seen[key]; exists {
			return ErrInvalidBundle
		}
		seen[key] = struct{}{}
		if file.RelativePath == "SKILL.md" && strings.TrimSpace(file.Content) != "" {
			hasSkill = true
		}
	}
	if !hasSkill || hasFileAncestor(seen) {
		return ErrInvalidBundle
	}
	return nil
}

func safeRelativePath(value string) bool {
	if value == "" || len([]byte(value)) > 1024 || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || len([]byte(component)) > 255 {
			return false
		}
	}
	return path.Clean(value) == value
}

func hasFileAncestor(files map[string]struct{}) bool {
	for value := range files {
		for parent := path.Dir(value); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return true
			}
		}
	}
	return false
}

func skillFileDigest(files []apicontract.LocalSessionSkillFile) string {
	ordered := append([]apicontract.LocalSessionSkillFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RelativePath < ordered[j].RelativePath })
	hash := sha256.New()
	for _, file := range ordered {
		_, _ = hash.Write([]byte(file.RelativePath))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(file.Content))
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validateSkillDigest(skill apicontract.LocalSessionSkill) error {
	if skillFileDigest(skill.Files) != skill.Digest {
		return fmt.Errorf("%w: skill digest mismatch", ErrInvalidBundle)
	}
	return nil
}
