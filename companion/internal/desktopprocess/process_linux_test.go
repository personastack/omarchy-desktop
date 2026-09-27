//go:build linux

package desktopprocess

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartStreamsOutputBeforeExitAndKeepsStreamsSeparate(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "printf first; sleep 1.3; printf second; printf problem >&2", t.TempDir(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(started.ExecutionID) != 36 || started.ExecutionID[14] != '4' || !strings.ContainsRune("89ab", rune(started.ExecutionID[19])) {
		t.Fatalf("execution ID is not a canonical random UUID: %q", started.ExecutionID)
	}
	if started.State != StateRunning || !strings.Contains(joinStream(started.Chunks, "stdout"), "first") {
		t.Fatalf("initial output/state = %#v", started)
	}
	all := append([]Chunk(nil), started.Chunks...)
	cursor := started.NextCursor
	deadline := time.Now().Add(5 * time.Second)
	var result Read
	for {
		result, err = manager.Read(context.Background(), started.ExecutionID, cursor, time.Until(deadline))
		if err != nil {
			t.Fatalf("read process output: %v", err)
		}
		all = append(all, result.Chunks...)
		cursor = result.NextCursor
		if result.State != StateRunning {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("process did not exit before deadline: state=%q", result.State)
		}
	}
	if result.State != StateExited {
		t.Fatalf("terminal process state = %q", result.State)
	}
	if !strings.Contains(joinStream(all, "stdout"), "second") || joinStream(all, "stderr") != "problem" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("output result = %#v, stdout=%q, stderr=%q", result, joinStream(all, "stdout"), joinStream(all, "stderr"))
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestValidateShellAllowsSupportedPOSIXShellsOnly(t *testing.T) {
	t.Parallel()
	if err := validateShell("/bin/sh"); err != nil {
		t.Fatalf("validate /bin/sh: %v", err)
	}
	if err := validateShell("/bin/echo"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("validate non-shell executable = %v", err)
	}
	if err := validateShell("sh"); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("validate relative shell = %v", err)
	}
}

func TestWriteAndCloseStdin(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "read answer; printf 'received:%s' \"$answer\"", t.TempDir(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(context.Background(), started.ExecutionID, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Read(context.Background(), started.ExecutionID, 0, 3*time.Second)
	if err != nil || result.State != StateExited || !strings.Contains(joinStream(result.Chunks, "stdout"), "received:hello") {
		t.Fatalf("stdin result = %#v, %v", result, err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestCloseStdinDeliversEOFAndRejectsFurtherWrites(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "cat >/dev/null; printf stdin-closed", t.TempDir(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseStdin(context.Background(), started.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(context.Background(), started.ExecutionID, []byte("late")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("write after EOF = %v", err)
	}
	chunks := append([]Chunk(nil), started.Chunks...)
	cursor := started.NextCursor
	deadline := time.Now().Add(3 * time.Second)
	var result Read
	for {
		result, err = manager.Read(context.Background(), started.ExecutionID, cursor, time.Until(deadline))
		if err != nil {
			t.Fatalf("read after stdin EOF: %#v, %v", result, err)
		}
		chunks = append(chunks, result.Chunks...)
		cursor = result.NextCursor
		if result.State != StateRunning {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("process did not exit after stdin EOF: %#v", result)
		}
	}
	if result.State != StateExited || !strings.Contains(joinStream(chunks, "stdout"), "stdin-closed") {
		t.Fatalf("EOF result = %#v, stdout=%q", result, joinStream(chunks, "stdout"))
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestCancelStopsManagedProcessGroup(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "sleep 20 & printf job-started; wait", t.TempDir(), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Cancel(context.Background(), started.ExecutionID); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Status(started.ExecutionID)
	if err != nil || result.State != StateCanceled {
		t.Fatalf("cancel status = %#v, %v", result, err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestManagedProcessLimit(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	for range MaxProcesses {
		if _, err := manager.Start(context.Background(), "printf ready; read answer", t.TempDir(), 20*time.Second); err != nil {
			t.Fatalf("start within process limit: %v", err)
		}
	}
	if _, err := manager.Start(context.Background(), "true", t.TempDir(), time.Second); !errors.Is(err, ErrTooManyProcesses) {
		t.Fatalf("start beyond process limit = %v", err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestCancellationUnblocksStdinWriter(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "sleep 20", t.TempDir(), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() {
		for range 4 {
			if writeErr := manager.Write(context.Background(), started.ExecutionID, make([]byte, MaxInputBytes)); writeErr != nil {
				writerDone <- writeErr
				return
			}
		}
		writerDone <- nil
	}()
	time.Sleep(100 * time.Millisecond)
	if err := manager.Cancel(context.Background(), started.ExecutionID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("blocked stdin writer did not stop after process cancellation")
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestCanceledStdinWriteReturnsWithoutStoppingProcess(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "sleep 20", t.TempDir(), 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	writeContext, cancelWrite := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() {
		for range 4 {
			if writeErr := manager.Write(writeContext, started.ExecutionID, make([]byte, MaxInputBytes)); writeErr != nil {
				writerDone <- writeErr
				return
			}
		}
		writerDone <- nil
	}()
	time.Sleep(100 * time.Millisecond)
	cancelWrite()
	select {
	case writeErr := <-writerDone:
		if !errors.Is(writeErr, context.Canceled) {
			t.Fatalf("canceled input write = %v", writeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("input write ignored its context")
	}
	status, err := manager.Status(started.ExecutionID)
	if err != nil || status.State != StateRunning {
		t.Fatalf("process after canceled write = %#v, %v", status, err)
	}
	if err := manager.Cancel(context.Background(), started.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestCanceledStartStopsTheUnreturnedProcess(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := manager.Start(ctx, "sleep 20", t.TempDir(), 20*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start = %v", err)
	}
	if got := manager.Diagnostics().ActiveProcesses; got != 0 {
		t.Fatalf("active processes after canceled start = %d", got)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestUnpinnedExitDrainsInheritedOutputPipes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	pidPath := directory + "/child.pid"
	manager := New("/bin/bash")
	waiting := make(chan struct{})
	continueWait := make(chan struct{})
	defer func() {
		select {
		case <-continueWait:
		default:
			close(continueWait)
		}
	}()
	manager.waitUnreaped = func(int) error {
		close(waiting)
		<-continueWait
		return errors.New("forced waitid failure")
	}
	started, err := manager.Start(context.Background(), "sleep 20 & echo $! > "+shellQuote(pidPath)+"; exit 0", directory, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("waitid hook was not called")
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || childPID <= 0 {
		t.Fatalf("child pid %q: %v", data, err)
	}
	defer func() { _ = syscall.Kill(childPID, syscall.SIGKILL) }()
	close(continueWait)
	deadline := time.Now().Add(processReaderDrainPeriod + 2*time.Second)
	for {
		status, statusErr := manager.Read(context.Background(), started.ExecutionID, 0, 0)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.State != StateRunning {
			if !status.OutputGap {
				t.Fatal("forced reader drain did not report incomplete output")
			}
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatal("session remained running after inherited-pipe drain deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTimeoutAndOutputGap(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "head -c 5000000 /dev/zero", t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		status, statusErr := manager.Status(started.ExecutionID)
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.State != StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process did not exit before deadline: state=%q", status.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	result, err := manager.Read(context.Background(), started.ExecutionID, 0, 0)
	if err != nil || result.State != StateExited || !result.OutputGap || result.Earliest <= 1 {
		t.Fatalf("buffer state=%q gap=%t earliest=%d err=%v", result.State, result.OutputGap, result.Earliest, err)
	}
	if got := chunkBytes(result.Chunks); got > MaxReadBytes {
		t.Fatalf("read bytes = %d, limit %d", got, MaxReadBytes)
	}
	latest, err := manager.Read(context.Background(), started.ExecutionID, result.NextCursor, 0)
	if err != nil || latest.OutputGap {
		t.Fatalf("read from retained cursor reports a gap: %#v, %v", latest, err)
	}
	if manager.Diagnostics().OutputGapsTotal == 0 {
		t.Fatal("output drop was not reported in diagnostics")
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestRejectsInvalidBoundsAndDirectory(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	if _, err := manager.Start(context.Background(), strings.Repeat("x", MaxCommandBytes+1), t.TempDir(), time.Second); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("command bound error = %v", err)
	}
	if _, err := manager.Start(context.Background(), "true", "relative", time.Second); !errors.Is(err, ErrInvalidWorkingDirectory) {
		t.Fatalf("relative directory error = %v", err)
	}
	started, err := manager.Start(context.Background(), "true", t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(context.Background(), started.ExecutionID, make([]byte, MaxInputBytes+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("input bound error = %v", err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func TestReadJSONMatchesMacProcessDTO(t *testing.T) {
	t.Parallel()
	code := 7
	payload, err := json.Marshal(Read{ExecutionID: "4ad3b23e-01cc-48da-8dc7-bc573adb3419",
		Chunks: []Chunk{{Sequence: 1, Stream: "stdout", Data: []byte("ok")}}, State: StateExited,
		ExitCode: &code})
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(payload, &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 8 || value["state"] != "exited" || value["signal"] != nil {
		t.Fatalf("process DTO fields = %s", payload)
	}
	chunks, ok := value["chunks"].([]any)
	if !ok || len(chunks) != 1 || chunks[0].(map[string]any)["data_base64"] != "b2s=" {
		t.Fatalf("process chunks = %s", payload)
	}
	if _, ok = value["execution_id"].(string); !ok || value["earliest_cursor"] == nil || value["next_cursor"] == nil || value["output_gap"] == nil || value["exit_code"] != float64(7) {
		t.Fatalf("process DTO values = %s", payload)
	}
}

func TestUnverifiedProcessGroupCleanupIsNotReportedAsSuccessful(t *testing.T) {
	t.Parallel()
	if waitForGroupExit(321, func(int) (bool, bool) { return false, false }) {
		t.Fatal("unverified group state was reported as clean")
	}
}

func TestUnconfirmedProcessRemainsCountedAndRetained(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	manager.sessions["unconfirmed"] = &session{id: "unconfirmed", state: StateCanceled}
	if got := manager.activeLocked(); got != 1 {
		t.Fatalf("unconfirmed active process count = %d", got)
	}
	manager.pruneLocked()
	if manager.sessions["unconfirmed"] == nil {
		t.Fatal("unconfirmed process session was pruned")
	}
}

func TestTimeoutStopsProcessGroup(t *testing.T) {
	t.Parallel()
	manager := New("/bin/bash")
	started, err := manager.Start(context.Background(), "sleep 20", t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Read(context.Background(), started.ExecutionID, 0, 4*time.Second)
	if err != nil || result.State != StateTimedOut {
		t.Fatalf("timeout result = %#v, %v", result, err)
	}
	if !manager.CloseAll(context.Background()) {
		t.Fatal("CloseAll did not confirm cleanup")
	}
}

func joinStream(chunks []Chunk, stream string) string {
	var result strings.Builder
	for _, chunk := range chunks {
		if chunk.Stream == stream {
			result.Write(chunk.Data)
		}
	}
	return result.String()
}

func chunkBytes(chunks []Chunk) int {
	count := 0
	for _, chunk := range chunks {
		count += len(chunk.Data)
	}
	return count
}
