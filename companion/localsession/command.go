package localsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
)

const maxBundleWireBytes = 12 * 1024 * 1024
const maxCommandWireBytes = maxBundleWireBytes + 1024*1024

var (
	personaIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	uuidPattern      = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type Action string

const (
	ActionState       Action = "state"
	ActionSelect      Action = "select_harness"
	ActionPrepare     Action = "prepare"
	ActionConfigure   Action = "configure"
	HarnessCodex      string = "codex"
	HarnessClaudeCode string = "claude_code"
)

var ErrInvalidRequest = errors.New("invalid local session request")
var ErrInvalidBundle = errors.New("invalid local session bundle")
var ErrUnsafeFiles = errors.New("PersonaStack cannot safely install local session files")
var ErrUnavailable = errors.New("local session setup is unavailable")

// Command is the finite native local-session message accepted from the hosted UI.
// Bundle remains raw until its authority-owned typed contract validates it.
type Command struct {
	Action    Action
	Scope     string
	Harness   string
	PersonaID string
	PendingID string
	Bundle    json.RawMessage
}

func Parse(raw []byte) (Command, error) {
	if len(raw) == 0 || len(raw) > maxCommandWireBytes || !utf8.Valid(raw) || !wirejson.ValidUniqueJSON(raw) {
		return Command{}, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil || fields == nil {
		return Command{}, ErrInvalidRequest
	}
	version, okVersion := stringField(fields, "version")
	action, okAction := stringField(fields, "action")
	scope, okScope := stringField(fields, "scope")
	if !okVersion || version != "1" || !okAction || !okScope || len([]byte(scope)) > 512 {
		return Command{}, ErrInvalidRequest
	}
	command := Command{Action: Action(action), Scope: scope}
	switch command.Action {
	case ActionState:
		if exactKeys(fields, "version", "action", "scope") {
			return command, nil
		}
	case ActionSelect:
		if !nonEmptyScope(scope) || !exactKeys(fields, "version", "action", "scope", "harness") {
			break
		}
		command.Harness, _ = stringField(fields, "harness")
		if validHarness(command.Harness) {
			return command, nil
		}
	case ActionPrepare:
		if !nonEmptyScope(scope) || !exactKeys(fields, "version", "action", "scope", "harness", "persona_id") {
			break
		}
		command.Harness, _ = stringField(fields, "harness")
		command.PersonaID, _ = stringField(fields, "persona_id")
		if validHarness(command.Harness) && personaIDPattern.MatchString(command.PersonaID) {
			return command, nil
		}
	case ActionConfigure:
		if !nonEmptyScope(scope) || !exactKeys(fields, "version", "action", "scope", "pending_id", "bundle") {
			break
		}
		command.PendingID, _ = stringField(fields, "pending_id")
		command.Bundle = bytes.Clone(fields["bundle"])
		var bundleObject map[string]json.RawMessage
		bundleErr := json.Unmarshal(command.Bundle, &bundleObject)
		if !uuidPattern.MatchString(command.PendingID) || len(command.Bundle) == 0 || !json.Valid(command.Bundle) || bundleErr != nil || bundleObject == nil {
			return Command{}, ErrInvalidRequest
		}
		bundleSize, err := normalizedJSONObjectSize(command.Bundle)
		if err != nil {
			return Command{}, ErrInvalidRequest
		}
		if bundleSize > maxBundleWireBytes {
			return Command{}, ErrInvalidBundle
		}
		return command, nil
	}
	return Command{}, ErrInvalidRequest
}

func normalizedJSONObjectSize(raw []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	if _, ok := value.(map[string]any); !ok {
		return 0, ErrInvalidRequest
	}
	var normalized bytes.Buffer
	encoder := json.NewEncoder(&normalized)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0, err
	}
	return normalized.Len() - 1, nil // json.Encoder appends one newline.
}

func decodeOne(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("invalid trailing JSON")
	}
	return nil
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func exactKeys(fields map[string]json.RawMessage, keys ...string) bool {
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func nonEmptyScope(scope string) bool { return scope != "" }

func validHarness(harness string) bool {
	return harness == HarnessCodex || harness == HarnessClaudeCode
}
