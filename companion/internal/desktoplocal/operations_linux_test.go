//go:build linux

package desktoplocal

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/desktopfiles"
	"github.com/personastack/omarchy-desktop/companion/internal/testfixture"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

func TestSharedDesktopParityImageAndUncertainWriteFixtures(t *testing.T) {
	t.Parallel()
	fixture, err := testfixture.LoadDesktopParity()
	if err != nil {
		t.Fatal(err)
	}
	operations := New("/bin/bash")
	t.Cleanup(func() { operations.CloseAll(context.Background()) })

	imageBytes, err := hex.DecodeString(fixture.Files.Image.BytesHex)
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), fixture.Files.Image.Name)
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	openArguments, err := json.Marshal(map[string]string{"action": "open", "path": imagePath})
	if err != nil {
		t.Fatal(err)
	}
	opened := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile, string(openArguments))
	blocks, ok := opened["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("image fixture content = %#v", opened["content"])
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || block["type"] != "image" || block["mimeType"] != fixture.Files.Image.MIMEType || block["data"] != base64.StdEncoding.EncodeToString(imageBytes) {
		t.Fatalf("image fixture block = %#v", blocks[0])
	}

	missingPath := filepath.Join(t.TempDir(), "missing", "uncertain.txt")
	writeArguments, err := json.Marshal(map[string]string{"action": "write", "path": missingPath, "mode": "append", "content_base64": ""})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile, writeArguments, time.Minute)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != fixture.Files.UncertainWrite.KnownFailureCode {
		t.Fatalf("known failed write fixture=%#v, want %q", err, fixture.Files.UncertainWrite.KnownFailureCode)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(writeArguments, &fields); err != nil {
		t.Fatal(err)
	}
	uncertain, ok := mapError(agentgatewayruntime.DesktopControlOperationFile, fields, desktopfiles.ErrWriteOutcomeUnknown).(*Failure)
	if !ok || uncertain.Code != fixture.Files.UncertainWrite.Code || uncertain.Message != fixture.Files.UncertainWrite.Message {
		t.Fatalf("uncertain-write mapping=%#v, want %q", uncertain, fixture.Files.UncertainWrite.Code)
	}

	if os.Geteuid() != 0 {
		permissionPath := filepath.Join(t.TempDir(), "restricted")
		if err := os.Mkdir(permissionPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(permissionPath, os.FileMode(fixture.Files.Permission.Mode)); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(permissionPath, 0o700)
		statArguments, err := json.Marshal(map[string]string{"action": "list", "path": permissionPath})
		if err != nil {
			t.Fatal(err)
		}
		_, err = operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile, statArguments, time.Minute)
		var permissionFailure *Failure
		if !errors.As(err, &permissionFailure) || permissionFailure.Code != fixture.Files.Permission.Code {
			t.Fatalf("permission fixture failure=%#v, want %q", err, fixture.Files.Permission.Code)
		}
	}
}

func TestFileOperationDTOsMatchDesktopContract(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := New("/bin/bash")
	opened := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"open","path":"`+path+`"}`)
	handle, ok := opened["handle"].(string)
	if !ok || len(handle) != 36 {
		t.Fatalf("file handle=%#v", opened["handle"])
	}
	if opened["encoding"] != "utf-8" || opened["content_text"] != "one\ntwo\n" || opened["line_count"] != float64(2) {
		t.Fatalf("opened file DTO=%#v", opened)
	}
	canonicalHandle := strings.ToUpper(handle)
	read := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"read","handle":"`+canonicalHandle+`","offset":4}`)
	if read["content_text"] != "two\n" || read["byte_offset"] != float64(4) || read["line_start"] != nil {
		t.Fatalf("read DTO=%#v", read)
	}
	read = callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"read","handle":"`+canonicalHandle+`","offset":0,"length":null}`)
	if read["content_text"] != "one\ntwo\n" {
		t.Fatalf("null length should use the macOS default: %#v", read)
	}
	closed := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"close","handle":"`+canonicalHandle+`"}`)
	if closed["closed"] != true {
		t.Fatalf("close DTO=%#v", closed)
	}
	stat := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"stat","path":"`+path+`"}`)
	if stat["kind"] != "file" || stat["symlink_target"] != nil || stat["modified_at"] == nil {
		t.Fatalf("stat DTO=%#v", stat)
	}
}

func TestMalformedDesktopControlArgumentsKeepContractErrorCodes(t *testing.T) {
	t.Parallel()
	operations := New("/bin/bash")
	t.Cleanup(func() { operations.CloseAll(context.Background()) })

	tests := []struct {
		name      string
		operation agentgatewayruntime.DesktopControlOperation
		arguments string
		wantCode  string
	}{
		{name: "malformed file handle", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"not-a-uuid"}`, wantCode: "invalid_arguments"},
		{name: "unknown file handle", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","offset":0}`, wantCode: "desktop_file_handle_expired"},
		{name: "malformed read offset", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","offset":-1}`, wantCode: "invalid_arguments"},
		{name: "missing read offset", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001"}`, wantCode: "invalid_arguments"},
		{name: "null read offset", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","offset":null}`, wantCode: "invalid_arguments"},
		{name: "missing line count", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","start_line":1}`, wantCode: "invalid_arguments"},
		{name: "null line count", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","start_line":1,"line_count":null}`, wantCode: "invalid_arguments"},
		{name: "null start line falls back to byte read", operation: agentgatewayruntime.DesktopControlOperationFile,
			arguments: `{"action":"read","handle":"00000000-0000-4000-8000-000000000001","start_line":null,"offset":0}`, wantCode: "desktop_file_handle_expired"},
		{name: "malformed execution id", operation: agentgatewayruntime.DesktopControlOperationShellStatus,
			arguments: `{"execution_id":"not-a-uuid"}`, wantCode: "invalid_arguments"},
		{name: "unknown execution id", operation: agentgatewayruntime.DesktopControlOperationShellStatus,
			arguments: `{"execution_id":"00000000-0000-4000-8000-000000000001"}`, wantCode: "desktop_process_handle_expired"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := operations.Call(context.Background(), test.operation, json.RawMessage(test.arguments), time.Minute)
			failure, ok := err.(*Failure)
			if !ok || failure.Code != test.wantCode {
				t.Fatalf("failure=%#v, want code %q", err, test.wantCode)
			}
		})
	}
}

func TestNumericDTOFieldsFallBackWhenTheirJSONTypesDoNotMatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "item.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "second.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := New("/bin/bash")
	result := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"list","path":"`+root+`","limit":1.0,"offset":false}`)
	entries, ok := result["entries"].([]any)
	if !ok || len(entries) != 1 || result["next_offset"] != float64(1) {
		t.Fatalf("list DTO with Foundation numeric casts=%#v", result)
	}
	result = callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"list","path":"`+root+`","limit":null,"offset":null}`)
	entries, ok = result["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("null list fields should use macOS defaults: %#v", result)
	}
}

func TestFoundationNumberCastsMatchDesktopDTOs(t *testing.T) {
	t.Parallel()
	fields := map[string]json.RawMessage{
		"integer_float":    json.RawMessage(`1.0`),
		"integer_bool":     json.RawMessage(`true`),
		"integer_fraction": json.RawMessage(`1.5`),
		"number_bool":      json.RawMessage(`true`),
		"bool_number":      json.RawMessage(`1`),
		"bool_two":         json.RawMessage(`2`),
	}
	for name, field := range map[string]string{"integer_float": "integer_float", "integer_bool": "integer_bool"} {
		if value, ok := integerField(fields, field, 0); !ok || value != 1 {
			t.Errorf("integer cast %s = %d, %v; want 1, true", name, value, ok)
		}
	}
	if value, ok := numberField(fields, "number_bool", 0); !ok || value != 1 {
		t.Errorf("number cast = %v, %v; want 1, true", value, ok)
	}
	if value, ok := boolField(fields, "bool_number"); !ok || !value {
		t.Errorf("boolean cast = %v, %v; want true, true", value, ok)
	}
	if value, ok := integerField(fields, "integer_fraction", 9); ok || value != 0 {
		t.Errorf("fractional integer cast = %d, %v; want 0, false", value, ok)
	}
	if value, ok := boolField(fields, "bool_two"); ok || value {
		t.Errorf("non-boolean number cast = %v, %v; want false, false", value, ok)
	}
}

func TestNullWriteContentIsRejectedWithoutChangingTheFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(path, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := New("/bin/bash")
	_, err := operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile,
		json.RawMessage(`{"action":"write","path":"`+path+`","content_base64":null,"mode":"replace"}`), time.Minute)
	failure, ok := err.(*Failure)
	if !ok || failure.Code != "invalid_arguments" {
		t.Fatalf("write failure=%#v, want invalid_arguments", err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil || string(content) != "preserve me" {
		t.Fatalf("file after rejected write=%q, read error=%v", content, readErr)
	}
}

func TestFoundationNumberWriteOffsetsPreservePartialWriteSemantics(t *testing.T) {
	t.Parallel()
	for _, offset := range []string{"true", "1.5"} {
		t.Run(offset, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "partial.txt")
			if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
				t.Fatal(err)
			}
			operations := New("/bin/bash")
			callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
				`{"action":"write","path":"`+path+`","content_base64":"WFk=","mode":"replace","offset":`+offset+`}`)
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "aXYdef" {
				t.Fatalf("file after offset=%s write=%q, error=%v", offset, content, err)
			}
		})
	}
}

func TestListAndSearchRangeErrorsMatchDesktopContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	operations := New("/bin/bash")
	for _, arguments := range []string{
		`{"action":"list","path":"` + root + `","limit":0}`,
		`{"action":"search","root":"` + root + `","name_contains":"item","limit":0}`,
	} {
		_, err := operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile, json.RawMessage(arguments), time.Minute)
		failure, ok := err.(*Failure)
		if !ok || failure.Code != "desktop_file_operation_failed" {
			t.Fatalf("range failure=%#v, want desktop_file_operation_failed", err)
		}
	}
}

func TestEmptySearchContinuationMatchesDesktopContract(t *testing.T) {
	t.Parallel()
	operations := New("/bin/bash")
	_, err := operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile,
		json.RawMessage(`{"action":"search","root":"`+t.TempDir()+`","name_contains":"item","continuation":""}`), time.Minute)
	failure, ok := err.(*Failure)
	if !ok || failure.Code != "desktop_file_search_continuation_expired" {
		t.Fatalf("empty continuation failure=%#v, want desktop_file_search_continuation_expired", err)
	}
	_, err = operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationFile,
		json.RawMessage(`{"action":"search","root":"`+filepath.Join(t.TempDir(), "missing")+`","name_contains":"item","continuation":""}`), time.Minute)
	failure, ok = err.(*Failure)
	if !ok || failure.Code != "desktop_file_operation_failed" {
		t.Fatalf("empty continuation with missing root failure=%#v, want desktop_file_operation_failed", err)
	}
}

func TestNonStringSearchContinuationMatchesDesktopContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "item.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := New("/bin/bash")
	result := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"search","root":"`+root+`","name_contains":"item","continuation":42}`)
	matches, ok := result["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("non-string continuation should behave as absent: %#v", result)
	}
}

func TestProcessOperationDTOsAndLeaseCleanup(t *testing.T) {
	t.Parallel()
	operations := New("/bin/bash")
	started := callObject(t, operations, agentgatewayruntime.DesktopControlOperationShellStart,
		`{"command":"sleep 20","working_directory":"`+t.TempDir()+`","timeout_seconds":30}`)
	id, ok := started["execution_id"].(string)
	if !ok || len(id) != 36 || started["state"] != "running" {
		t.Fatalf("start DTO=%#v", started)
	}
	statusDTO := callObject(t, operations, agentgatewayruntime.DesktopControlOperationShellStatus,
		`{"execution_id":"`+strings.ToUpper(id)+`"}`)
	if statusDTO["state"] != "running" {
		t.Fatalf("uppercase UUID status DTO=%#v", statusDTO)
	}
	if !operations.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm process cleanup")
	}
	status, err := operations.Call(context.Background(), agentgatewayruntime.DesktopControlOperationShellStatus,
		json.RawMessage(`{"execution_id":"`+id+`"}`), time.Second)
	if err == nil || len(status) != 0 {
		t.Fatalf("old process remained available after cleanup: result=%s err=%v", status, err)
	}
	started = callObject(t, operations, agentgatewayruntime.DesktopControlOperationShellStart,
		`{"command":"printf ready","working_directory":"`+t.TempDir()+`","timeout_seconds":5}`)
	if started["execution_id"] == "" {
		t.Fatalf("process manager did not restart after cleanup: %#v", started)
	}
	if !operations.CloseAll(context.Background()) {
		t.Fatal("second CloseAll did not confirm cleanup")
	}
}

func TestProcessReadAndWriteKeepOutputStreamsSeparate(t *testing.T) {
	t.Parallel()
	operations := New("/bin/bash")
	started := callObject(t, operations, agentgatewayruntime.DesktopControlOperationShellStart,
		`{"command":"printf out; printf err >&2","working_directory":"`+t.TempDir()+`","timeout_seconds":5}`)
	id := started["execution_id"].(string)
	deadline := time.Now().Add(3 * time.Second)
	for {
		read := callObject(t, operations, agentgatewayruntime.DesktopControlOperationShellRead,
			`{"execution_id":"`+id+`","cursor":0,"wait_ms":250}`)
		chunks, ok := read["chunks"].([]any)
		if !ok {
			t.Fatalf("process read chunks=%#v", read["chunks"])
		}
		streams := map[string]string{}
		for _, item := range chunks {
			chunk := item.(map[string]any)
			data, decodeErr := base64.StdEncoding.DecodeString(chunk["data_base64"].(string))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			streams[chunk["stream"].(string)] += string(data)
		}
		if strings.Contains(streams["stdout"], "out") && strings.Contains(streams["stderr"], "err") && read["state"] == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process streams did not complete: %#v", read)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !operations.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func callObject(t *testing.T, operations *Operations, operation agentgatewayruntime.DesktopControlOperation, arguments string) map[string]any {
	t.Helper()
	result, err := operations.Call(context.Background(), operation, json.RawMessage(arguments), time.Minute)
	if err != nil {
		t.Fatalf("operation %s: %v", operation, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("decode operation %s response %s: %v", operation, result, err)
	}
	return decoded
}
