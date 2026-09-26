//go:build linux

package desktoplocal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

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
	read := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"read","handle":"`+handle+`","offset":4}`)
	if read["content_text"] != "two\n" || read["byte_offset"] != float64(4) || read["line_start"] != nil {
		t.Fatalf("read DTO=%#v", read)
	}
	closed := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"close","handle":"`+handle+`"}`)
	if closed["closed"] != true {
		t.Fatalf("close DTO=%#v", closed)
	}
	stat := callObject(t, operations, agentgatewayruntime.DesktopControlOperationFile,
		`{"action":"stat","path":"`+path+`"}`)
	if stat["kind"] != "file" || stat["symlink_target"] != nil || stat["modified_at"] == nil {
		t.Fatalf("stat DTO=%#v", stat)
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
