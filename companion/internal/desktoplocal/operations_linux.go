//go:build linux

package desktoplocal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/desktopfiles"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopprocess"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

const maxImageBytes = 5 * 1024 * 1024

type Operations struct {
	mu        sync.Mutex
	files     *desktopfiles.OpenFiles
	processes *desktopprocess.Manager
	shell     string
}

type Failure struct {
	Code    string
	Message string
}

func (failure *Failure) Error() string              { return failure.Message }
func (failure *Failure) DesktopControlCode() string { return failure.Code }
func (failure *Failure) DesktopControlMessage() string {
	return failure.Message
}

func New(shell string) *Operations {
	return &Operations{files: desktopfiles.New(), processes: desktopprocess.New(shell), shell: shell}
}

func (operations *Operations) Call(ctx context.Context, operation agentgatewayruntime.DesktopControlOperation, arguments json.RawMessage, leaseRemaining time.Duration) (json.RawMessage, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fields, err := decodeObject(arguments)
	if err != nil {
		return nil, failure("invalid_arguments", "The Desktop Control tool arguments are invalid.")
	}
	var result any
	switch operation {
	case agentgatewayruntime.DesktopControlOperationFile:
		result, err = operations.file(ctx, fields)
	case agentgatewayruntime.DesktopControlOperationShellStart,
		agentgatewayruntime.DesktopControlOperationShellRead,
		agentgatewayruntime.DesktopControlOperationShellWrite,
		agentgatewayruntime.DesktopControlOperationShellStatus,
		agentgatewayruntime.DesktopControlOperationShellCancel:
		result, err = operations.process(ctx, operation, fields, leaseRemaining)
	default:
		return nil, failure("invalid_arguments", "The Desktop Control operation is not supported by the local executor.")
	}
	if err != nil {
		return nil, mapError(operation, fields, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, failure("desktop_command_failed", "The desktop command failed.")
	}
	return encoded, nil
}

func (operations *Operations) ActiveProcesses() int {
	operations.mu.Lock()
	processes := operations.processes
	operations.mu.Unlock()
	if processes == nil {
		return 0
	}
	return processes.Diagnostics().ActiveProcesses
}

func (operations *Operations) CloseAll(ctx context.Context) bool {
	operations.mu.Lock()
	processes := operations.processes
	operations.mu.Unlock()
	operations.files.CloseAll()
	if processes == nil || !processes.CloseAll(ctx) {
		return processes == nil
	}
	operations.mu.Lock()
	if operations.processes == processes {
		operations.processes = desktopprocess.New(operations.shell)
	}
	operations.mu.Unlock()
	return true
}

func (operations *Operations) file(ctx context.Context, fields map[string]json.RawMessage) (any, error) {
	action, ok := stringField(fields, "action")
	if !ok {
		return nil, desktopfiles.ErrInvalidPath
	}
	switch action {
	case "stat":
		path, ok := stringField(fields, "path")
		if !ok {
			return nil, desktopfiles.ErrInvalidPath
		}
		entry, err := desktopfiles.Metadata(path)
		return entryResult(entry), err
	case "list":
		path, ok := stringField(fields, "path")
		if !ok {
			return nil, desktopfiles.ErrInvalidPath
		}
		offset, ok := integerField(fields, "offset", 0)
		if !ok {
			return nil, desktopfiles.ErrInvalidRange
		}
		limit, ok := integerField(fields, "limit", 100)
		if !ok {
			return nil, desktopfiles.ErrInvalidRange
		}
		entries, next, err := desktopfiles.List(path, offset, limit)
		mapped := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			mapped = append(mapped, entryResult(entry))
		}
		return map[string]any{"entries": mapped, "next_offset": nullableInt(next)}, err
	case "search":
		root, ok := stringField(fields, "root")
		if !ok {
			return nil, desktopfiles.ErrNotDirectory
		}
		limit, ok := integerField(fields, "limit", 100)
		if !ok {
			return nil, desktopfiles.ErrInvalidRange
		}
		page, err := operations.files.Search(root, optionalString(fields, "name_contains"), optionalString(fields, "name_glob"),
			optionalString(fields, "content_contains"), limit, optionalString(fields, "continuation"))
		matches := make([]map[string]any, 0, len(page.Matches))
		for _, match := range page.Matches {
			entry := entryResult(match.Entry)
			entry["matched_lines"] = match.MatchedLines
			entry["content_scan_truncated"] = match.ContentScanTruncated
			matches = append(matches, entry)
		}
		return map[string]any{"matches": matches, "continuation": nullableString(page.Continuation),
			"incomplete_content_paths": page.IncompleteContentPaths, "complete": page.Complete}, err
	case "open":
		path, ok := stringField(fields, "path")
		if !ok {
			return nil, desktopfiles.ErrInvalidPath
		}
		opened, err := operations.files.Open(path)
		if err != nil {
			return nil, err
		}
		result := readResult(opened.Read)
		result["handle"] = opened.Handle
		result["size"] = opened.Size
		result["modified_at"] = isoTime(opened.ModifiedAt)
		result["revision"] = opened.Revision
		result["changed_since_open"] = opened.ChangedSinceOpen
		if imageMIME(result["mime_type"].(string)) && opened.Size <= maxImageBytes {
			image, changed, complete, readErr := collectImage(operations.files, opened)
			if readErr != nil {
				_ = operations.files.Close(opened.Handle)
				return nil, readErr
			}
			if complete && !changed {
				result["content"] = []map[string]string{{"type": "image", "data": base64.StdEncoding.EncodeToString(image), "mimeType": result["mime_type"].(string)}}
				result["byte_length"] = len(image)
				result["next_offset"] = len(image)
				result["end_of_file"] = true
				result["encoding"] = "image"
				delete(result, "content_base64")
				delete(result, "content_text")
				delete(result, "line_count")
			} else {
				reason := "incomplete_read"
				if changed {
					reason = "file_changed_during_read"
				}
				result["image_content_unavailable"] = reason
			}
		}
		return result, nil
	case "read":
		handle, ok := stringField(fields, "handle")
		if !ok {
			return nil, desktopfiles.ErrMissingHandle
		}
		length, ok := integerField(fields, "length", desktopfiles.MaxReadBytes)
		if !ok {
			return nil, desktopfiles.ErrInvalidRange
		}
		read, err := readFileOperation(operations.files, fields, handle, length)
		return readResult(read), err
	case "close":
		handle, ok := stringField(fields, "handle")
		if !ok {
			return nil, desktopfiles.ErrMissingHandle
		}
		if err := operations.files.Close(handle); err != nil {
			return nil, err
		}
		return map[string]bool{"closed": true}, nil
	case "write":
		path, pathOK := stringField(fields, "path")
		content, contentOK := stringField(fields, "content_base64")
		modeText, modeOK := stringField(fields, "mode")
		data, decodeErr := base64.StdEncoding.DecodeString(content)
		if !pathOK || !contentOK || decodeErr != nil || !modeOK {
			return nil, desktopfiles.ErrInvalidRange
		}
		mode := desktopfiles.WriteMode(modeText)
		if mode != desktopfiles.WriteCreate && mode != desktopfiles.WriteReplace && mode != desktopfiles.WriteAppend {
			return nil, desktopfiles.ErrInvalidRange
		}
		offset, ok := optionalInt64(fields, "offset")
		if !ok {
			return nil, desktopfiles.ErrInvalidRange
		}
		entry, err := desktopfiles.Write(path, data, mode, offset)
		return entryResult(entry), err
	case "patch":
		path, pathOK := stringField(fields, "path")
		expected, expectedOK := stringField(fields, "expected")
		replacement, replacementOK := stringField(fields, "replacement")
		if !pathOK || !expectedOK || !replacementOK {
			return nil, desktopfiles.ErrInvalidPath
		}
		entry, err := desktopfiles.Patch(path, expected, replacement)
		return entryResult(entry), err
	case "mkdir":
		path, ok := stringField(fields, "path")
		if !ok {
			return nil, desktopfiles.ErrInvalidPath
		}
		return map[string]bool{"created": true}, desktopfiles.MakeDirectory(path)
	case "move":
		source, sourceOK := stringField(fields, "source")
		destination, destinationOK := stringField(fields, "destination")
		if !sourceOK || !destinationOK {
			return nil, desktopfiles.ErrInvalidPath
		}
		return map[string]bool{"moved": true}, desktopfiles.Move(source, destination)
	case "remove":
		path, ok := stringField(fields, "path")
		if !ok {
			return nil, desktopfiles.ErrInvalidPath
		}
		return map[string]bool{"removed": true}, desktopfiles.Remove(path)
	default:
		return nil, desktopfiles.ErrInvalidRange
	}
}

func (operations *Operations) process(ctx context.Context, operation agentgatewayruntime.DesktopControlOperation, fields map[string]json.RawMessage, leaseRemaining time.Duration) (any, error) {
	operations.mu.Lock()
	processes := operations.processes
	operations.mu.Unlock()
	if processes == nil {
		return nil, desktopprocess.ErrMissingExecution
	}
	switch operation {
	case agentgatewayruntime.DesktopControlOperationShellStart:
		command, commandOK := stringField(fields, "command")
		workingDirectory, directoryOK := stringField(fields, "working_directory")
		timeoutSeconds, timeoutOK := numberField(fields, "timeout_seconds", 300)
		if !commandOK || !directoryOK || !timeoutOK {
			return nil, desktopprocess.ErrInvalidCommand
		}
		if timeoutSeconds < 1 {
			timeoutSeconds = 1
		}
		if timeoutSeconds > desktopprocess.MaxTimeout.Seconds() {
			timeoutSeconds = desktopprocess.MaxTimeout.Seconds()
		}
		if leaseRemaining < time.Second {
			return nil, failure("desktop_control_required", "The Desktop Control lease is expiring. Acquire it again before starting a command.")
		}
		timeout := time.Duration(timeoutSeconds * float64(time.Second))
		if timeout > leaseRemaining {
			timeout = leaseRemaining
		}
		read, err := processes.Start(ctx, command, workingDirectory, timeout)
		return read, err
	case agentgatewayruntime.DesktopControlOperationShellRead:
		id, ok := stringField(fields, "execution_id")
		if !ok {
			return nil, desktopprocess.ErrMissingExecution
		}
		cursor, ok := uint64Field(fields, "cursor", 0)
		if !ok {
			return nil, desktopprocess.ErrInvalidInput
		}
		waitMS, ok := integerField(fields, "wait_ms", 0)
		if !ok || waitMS < 0 {
			return nil, desktopprocess.ErrInvalidInput
		}
		read, err := processes.Read(ctx, id, cursor, time.Duration(waitMS)*time.Millisecond)
		return read, err
	case agentgatewayruntime.DesktopControlOperationShellStatus:
		id, ok := stringField(fields, "execution_id")
		if !ok {
			return nil, desktopprocess.ErrMissingExecution
		}
		return processes.Status(id)
	case agentgatewayruntime.DesktopControlOperationShellWrite:
		id, ok := stringField(fields, "execution_id")
		if !ok {
			return nil, desktopprocess.ErrMissingExecution
		}
		if closed, ok := boolField(fields, "close_stdin"); ok && closed {
			if err := processes.CloseStdin(ctx, id); err != nil {
				return nil, err
			}
		} else if interrupted, ok := boolField(fields, "interrupt"); ok && interrupted {
			if err := processes.Interrupt(id); err != nil {
				return nil, err
			}
		} else if encoded, ok := stringField(fields, "data_base64"); ok {
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, desktopprocess.ErrInvalidInput
			}
			if err := processes.Write(ctx, id, data); err != nil {
				return nil, err
			}
		} else {
			return nil, desktopprocess.ErrInvalidInput
		}
		return map[string]bool{"accepted": true}, nil
	case agentgatewayruntime.DesktopControlOperationShellCancel:
		id, ok := stringField(fields, "execution_id")
		if !ok {
			return nil, desktopprocess.ErrMissingExecution
		}
		if err := processes.Cancel(ctx, id); err != nil {
			return nil, err
		}
		return map[string]bool{"cancelled": true}, nil
	default:
		return nil, desktopprocess.ErrInvalidCommand
	}
}

func readFileOperation(files *desktopfiles.OpenFiles, fields map[string]json.RawMessage, handle string, length int) (desktopfiles.Read, error) {
	_, hasLine := fields["start_line"]
	if hasLine {
		if _, hasOffset := fields["offset"]; hasOffset {
			return desktopfiles.Read{}, desktopfiles.ErrInvalidRange
		}
		line, ok := integerField(fields, "start_line", 0)
		lineCount, countOK := integerField(fields, "line_count", 0)
		if !ok || !countOK || line < 1 || lineCount < 1 {
			return desktopfiles.Read{}, desktopfiles.ErrInvalidRange
		}
		return files.ReadLines(handle, line, lineCount, length)
	}
	if _, exists := fields["line_count"]; exists {
		return desktopfiles.Read{}, desktopfiles.ErrInvalidRange
	}
	offset, ok := int64Field(fields, "offset", 0)
	if !ok {
		return desktopfiles.Read{}, desktopfiles.ErrInvalidRange
	}
	return files.Read(handle, offset, length)
}

func collectImage(files *desktopfiles.OpenFiles, opened desktopfiles.Opened) ([]byte, bool, bool, error) {
	data := make([]byte, 0, int(opened.Size))
	data, _ = base64.StdEncoding.DecodeString(opened.ContentBase64)
	changed := opened.ChangedSinceOpen
	offset := opened.NextOffset
	for !opened.EndOfFile && offset < opened.Size {
		page, err := files.Read(opened.Handle, offset, desktopfiles.MaxReadBytes)
		if err != nil {
			return nil, changed, false, err
		}
		if len(page.ContentBase64) == 0 || page.NextOffset <= offset {
			return data, changed, false, nil
		}
		chunk, err := base64.StdEncoding.DecodeString(page.ContentBase64)
		if err != nil {
			return nil, changed, false, err
		}
		data = append(data, chunk...)
		changed = changed || page.ChangedSinceOpen
		offset = page.NextOffset
		if page.EndOfFile {
			break
		}
	}
	return data, changed, int64(len(data)) == opened.Size, nil
}

func readResult(read desktopfiles.Read) map[string]any {
	result := map[string]any{"path": read.Path, "byte_offset": read.ByteOffset, "byte_length": read.ByteLength,
		"next_offset": read.NextOffset, "end_of_file": read.EndOfFile, "changed_since_open": read.ChangedSinceOpen,
		"line_start": nullableIntValue(read.LineStart), "next_line": nullableIntValue(read.NextLine), "truncated": read.Truncated,
		"mime_type": mimeType(read.Path), "encoding": read.Encoding, "content_base64": read.ContentBase64}
	if read.ContentText != "" || read.Encoding == "utf-8" {
		result["content_text"] = read.ContentText
		result["line_count"] = lineCount(read.ContentText)
	} else {
		result["line_count"] = nil
	}
	if imageMIME(result["mime_type"].(string)) && read.ByteOffset == 0 && read.EndOfFile {
		result["content"] = []map[string]string{{"type": "image", "data": read.ContentBase64, "mimeType": result["mime_type"].(string)}}
	}
	return result
}

func entryResult(entry desktopfiles.Entry) map[string]any {
	return map[string]any{"path": entry.Path, "name": entry.Name, "kind": entry.Kind, "size": entry.Size,
		"modified_at": isoTime(entry.ModifiedAt), "symlink_target": nullableString(entry.SymlinkTarget)}
}

func mapError(operation agentgatewayruntime.DesktopControlOperation, fields map[string]json.RawMessage, err error) error {
	var code, message string
	switch {
	case errors.Is(err, desktopfiles.ErrPermissionDenied):
		code, message = "desktop_file_permission_denied", "Linux denied access to this file or directory. Choose a path your account can access."
	case errors.Is(err, desktopfiles.ErrMissingHandle):
		code, message = "desktop_file_handle_expired", "This file handle expired. Open the file again before reading it."
	case errors.Is(err, desktopfiles.ErrInvalidRange):
		code, message = "desktop_file_range_invalid", "The requested file range is outside the supported limit."
	case errors.Is(err, desktopfiles.ErrTooManyOpenFiles):
		code, message = "desktop_file_handle_limit", "Too many files are open for this desktop control session. Close a file handle and retry."
	case errors.Is(err, desktopfiles.ErrPatchMismatch):
		code, message = "desktop_file_changed", "The file changed or the expected text did not match. Read the current file before editing it again."
	case errors.Is(err, desktopfiles.ErrSearchIncomplete):
		code, message = "desktop_file_search_incomplete", "The file search exceeded its scan limit. Narrow the search to a smaller folder."
	case errors.Is(err, desktopfiles.ErrSearchContinuationExpired):
		code, message = "desktop_file_search_continuation_expired", "This file search continuation expired. Start the search again."
	case errors.Is(err, desktopfiles.ErrTooManySearches):
		code, message = "desktop_file_search_limit", "Too many file searches are still open. Continue or restart an earlier search."
	case errors.Is(err, desktopfiles.ErrInvalidPath), errors.Is(err, desktopfiles.ErrNotRegularFile),
		errors.Is(err, desktopfiles.ErrNotDirectory), errors.Is(err, desktopfiles.ErrContentTooLarge),
		errors.Is(err, desktopfiles.ErrDestinationExists), errors.Is(err, desktopfiles.ErrMetadataTooLarge),
		errors.Is(err, desktopfiles.ErrMetadataUnsupported), errors.Is(err, desktopfiles.ErrSessionEnded):
		code, message = "desktop_file_operation_failed", "PersonaStack Desktop could not complete this file operation. Check the path, file type, and operation limits."
	case errors.Is(err, desktopprocess.ErrPermissionDenied):
		code, message = "desktop_process_permission_denied", "Linux denied access to the command or working directory. Choose a location your account can access."
	case errors.Is(err, desktopprocess.ErrInvalidWorkingDirectory):
		code, message = "desktop_process_working_directory_invalid", "The command working directory is missing or is not an accessible directory."
	case errors.Is(err, desktopprocess.ErrTooManyProcesses):
		code, message = "desktop_process_limit", "The desktop already has the maximum number of managed commands running."
	case errors.Is(err, desktopprocess.ErrMissingExecution):
		code, message = "desktop_process_handle_expired", "This command session is no longer available. Start a new command."
	case errors.Is(err, desktopprocess.ErrInvalidInput):
		code, message = "desktop_process_input_invalid", "The command input is invalid or exceeds the supported size."
	case errors.Is(err, desktopprocess.ErrCleanupUnconfirmed):
		code, message = "desktop_process_cancel_unconfirmed", "The desktop could not confirm that the command stopped. Check its status before retrying."
	case errors.Is(err, desktopprocess.ErrInvalidCommand):
		code, message = "desktop_process_start_failed", "The desktop could not start the command. Check the working directory and try again."
	default:
		code, message = "desktop_command_failed", "The desktop command failed."
	}
	if operation == agentgatewayruntime.DesktopControlOperationFile && isPartialWrite(fields, err) {
		code, message = "desktop_file_write_outcome_unknown", "The file may have changed before the write stopped. Read it again before retrying."
	}
	return failure(code, message)
}

func isPartialWrite(fields map[string]json.RawMessage, err error) bool {
	action, _ := stringField(fields, "action")
	if action != "write" || errors.Is(err, desktopfiles.ErrPermissionDenied) || errors.Is(err, desktopfiles.ErrInvalidRange) || errors.Is(err, desktopfiles.ErrContentTooLarge) {
		return false
	}
	mode, _ := stringField(fields, "mode")
	return mode == "create" || mode == "append" || fields["offset"] != nil
}

func failure(code, message string) *Failure { return &Failure{Code: code, Message: message} }

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if !wirejson.ValidUniqueJSON(raw) {
		return nil, errors.New("invalid JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid JSON object")
	}
	return fields, nil
}

func stringField(fields map[string]json.RawMessage, name string) (string, bool) {
	var value string
	raw, ok := fields[name]
	if !ok || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func optionalString(fields map[string]json.RawMessage, name string) string {
	value, _ := stringField(fields, name)
	return value
}

func integerField(fields map[string]json.RawMessage, name string, fallback int) (int, bool) {
	raw, ok := fields[name]
	if !ok {
		return fallback, true
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func int64Field(fields map[string]json.RawMessage, name string, fallback int64) (int64, bool) {
	raw, ok := fields[name]
	if !ok {
		return fallback, true
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func optionalInt64(fields map[string]json.RawMessage, name string) (*int64, bool) {
	raw, ok := fields[name]
	if !ok {
		return nil, true
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false
	}
	return &value, true
}

func uint64Field(fields map[string]json.RawMessage, name string, fallback uint64) (uint64, bool) {
	raw, ok := fields[name]
	if !ok {
		return fallback, true
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return 0, false
	}
	return uint64(value), true
}

func numberField(fields map[string]json.RawMessage, name string, fallback float64) (float64, bool) {
	raw, ok := fields[name]
	if !ok {
		return fallback, true
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func boolField(fields map[string]json.RawMessage, name string) (bool, bool) {
	raw, ok := fields[name]
	if !ok {
		return false, true
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func isoTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000Z") }

func mimeType(path string) string {
	value := mime.TypeByExtension(filepath.Ext(path))
	if value == "" {
		return "application/octet-stream"
	}
	return strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
}

func imageMIME(value string) bool {
	return value == "image/png" || value == "image/jpeg" || value == "image/webp"
}

func lineCount(value string) int {
	if value == "" {
		return 0
	}
	count := strings.Count(value, "\n")
	if !strings.HasSuffix(value, "\n") {
		count++
	}
	return count
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableStringValue(value string) any { return nullableString(value) }

func nullableIntValue(value int) any {
	if value == 0 {
		return nil
	}
	return value
}
