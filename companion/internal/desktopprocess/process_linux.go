//go:build linux

package desktopprocess

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	MaxCommandBytes          = 64 * 1024
	MaxInputBytes            = 32 * 1024
	MaxReadBytes             = 1 * 1024 * 1024
	MaxBufferedBytesPerRun   = 4 * 1024 * 1024
	MaxQueuedInputBytes      = 128 * 1024
	MaxProcesses             = 4
	MaxRetainedSessions      = 16
	MaxTimeout               = 30 * time.Minute
	MaxReadWait              = 10 * time.Second
	processOutputChunkBytes  = 32 * 1024
	processCancelGrace       = 250 * time.Millisecond
	processCleanupTimeout    = 3500 * time.Millisecond
	processReaderDrainPeriod = 2 * time.Second
)

var (
	ErrInvalidCommand          = errors.New("invalid process command")
	ErrInvalidWorkingDirectory = errors.New("invalid process working directory")
	ErrPermissionDenied        = errors.New("process permission denied")
	ErrTooManyProcesses        = errors.New("too many active processes")
	ErrMissingExecution        = errors.New("process execution is unavailable")
	ErrInvalidInput            = errors.New("invalid process input")
	ErrCleanupUnconfirmed      = errors.New("process group cleanup is unconfirmed")
)

type State string

const (
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateCanceled State = "cancelled"
	StateTimedOut State = "timedOut"
)

type Chunk struct {
	Sequence uint64 `json:"sequence"`
	Stream   string `json:"stream"`
	Data     []byte `json:"data_base64"`
}

type Read struct {
	ExecutionID string  `json:"execution_id"`
	Chunks      []Chunk `json:"chunks"`
	NextCursor  uint64  `json:"next_cursor"`
	Earliest    uint64  `json:"earliest_cursor"`
	OutputGap   bool    `json:"output_gap"`
	State       State   `json:"state"`
	ExitCode    *int    `json:"exit_code"`
	Signal      *int    `json:"signal"`
}

type Diagnostics struct {
	ActiveProcesses     int    `json:"active_processes"`
	BufferedOutputBytes int    `json:"buffered_output_bytes"`
	OutputGapsTotal     uint64 `json:"output_gaps_total"`
}

type Manager struct {
	shell        string
	waitUnreaped func(int) error

	mu         sync.Mutex
	sessions   map[string]*session
	closed     bool
	outputGaps uint64
}

type session struct {
	id        string
	command   *exec.Cmd
	stdin     *os.File
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	writeGate chan struct{}
	stdinErr  error

	chunks          []Chunk
	buffered        int
	sequence        uint64
	incomplete      bool
	readers         int
	leaderExited    bool
	groupPinned     bool
	cleanupFinished bool
	cleanupOK       bool
	requested       State
	terminal        State
	state           State
	exitCode        *int
	signal          *int
	queuedInput     int
	startedAt       time.Time
	timer           *time.Timer
	done            chan struct{}
	doneOnce        sync.Once
}

// New creates a process manager for the selected user's login shell. The shell
// must be an absolute executable path, such as /bin/zsh on Omarchy.
func New(shell string) *Manager {
	return &Manager{shell: shell, waitUnreaped: waitUnreaped, sessions: make(map[string]*session)}
}

func (m *Manager) Start(ctx context.Context, command, workingDirectory string, timeout time.Duration) (Read, error) {
	if ctx == nil || strings.TrimSpace(command) == "" || len(command) > MaxCommandBytes {
		return Read{}, ErrInvalidCommand
	}
	if !filepath.IsAbs(workingDirectory) || len(workingDirectory) > 4096 {
		return Read{}, ErrInvalidWorkingDirectory
	}
	if err := ctx.Err(); err != nil {
		return Read{}, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	if err := validateShell(m.shell); err != nil {
		return Read{}, err
	}
	if err := validateDirectory(workingDirectory); err != nil {
		return Read{}, err
	}
	id, err := randomID()
	if err != nil {
		return Read{}, err
	}
	script := "cd -- " + shellQuote(workingDirectory) +
		" || exit $?; trap 'wait; exit 143' TERM; trap 'wait; exit 130' INT; " +
		command + "; _personastack_status=$?; wait; exit $_personastack_status"
	cmd := exec.Command(m.shell, "-lc", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = childEnvironment(os.Environ())
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return Read{}, commandError(err)
	}
	stdin, ok := stdinPipe.(*os.File)
	if !ok {
		_ = stdinPipe.Close()
		return Read{}, ErrInvalidCommand
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return Read{}, commandError(err)
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return Read{}, commandError(err)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return Read{}, ErrMissingExecution
	}
	m.pruneLocked()
	if m.activeLocked() >= MaxProcesses {
		m.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return Read{}, ErrTooManyProcesses
	}
	if err = cmd.Start(); err != nil {
		m.mu.Unlock()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return Read{}, commandError(err)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	current := &session{id: id, command: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		writeGate: make(chan struct{}, 1), chunks: make([]Chunk, 0), readers: 2,
		state: StateRunning, startedAt: time.Now(), done: make(chan struct{})}
	current.writeGate <- struct{}{}
	current.timer = time.AfterFunc(timeout, func() { m.terminate(id, StateTimedOut) })
	m.sessions[id] = current
	m.mu.Unlock()
	var startCanceled atomic.Bool
	stopCancellation := context.AfterFunc(ctx, func() {
		startCanceled.Store(true)
		m.terminate(id, StateCanceled)
	})
	go m.readOutput(id, "stdout", stdout)
	go m.readOutput(id, "stderr", stderr)
	go m.waitLeader(id, cmd)
	initial, readErr := m.Read(ctx, id, 0, time.Second)
	stopEffective := stopCancellation()
	if readErr != nil || ctx.Err() != nil || startCanceled.Load() || !stopEffective {
		cleanup, cancel := context.WithTimeout(context.Background(), processCleanupTimeout+processCancelGrace+time.Second)
		defer cancel()
		if cleanupErr := m.Cancel(cleanup, id); cleanupErr != nil {
			m.mu.Lock()
			m.closed = true
			m.mu.Unlock()
			return Read{}, ErrCleanupUnconfirmed
		}
		if readErr != nil {
			return Read{}, readErr
		}
		return Read{}, ctx.Err()
	}
	stopCancellation()
	return initial, nil
}

func (m *Manager) Read(ctx context.Context, id string, cursor uint64, wait time.Duration) (Read, error) {
	if ctx == nil {
		return Read{}, ErrMissingExecution
	}
	if wait < 0 {
		wait = 0
	}
	if wait > MaxReadWait {
		wait = MaxReadWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		current := m.sessions[id]
		if current == nil {
			m.mu.Unlock()
			return Read{}, ErrMissingExecution
		}
		result := current.read(cursor)
		ready := result.NextCursor > cursor || current.state != StateRunning
		m.mu.Unlock()
		if ready || wait == 0 {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return Read{}, ctx.Err()
		case <-deadline.C:
			return result, nil
		case <-ticker.C:
		}
	}
}

func (m *Manager) Status(id string) (Read, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[id]
	if current == nil {
		return Read{}, ErrMissingExecution
	}
	result := current.read(0)
	result.Chunks = []Chunk{}
	result.NextCursor = 0
	result.OutputGap = false
	return result, nil
}

func (m *Manager) Write(ctx context.Context, id string, data []byte) error {
	if ctx == nil || len(data) == 0 || len(data) > MaxInputBytes {
		return ErrInvalidInput
	}
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil || current.state != StateRunning || current.stdinErr != nil || current.queuedInput+len(data) > MaxQueuedInputBytes {
		m.mu.Unlock()
		return ErrInvalidInput
	}
	current.queuedInput += len(data)
	gate, done := current.writeGate, current.done
	input := current.stdin
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if live := m.sessions[id]; live != nil {
			live.queuedInput -= len(data)
		}
		m.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrInvalidInput
	case <-gate:
	}
	defer func() { gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeWithContext(ctx, done, input, data); err != nil {
		return commandError(err)
	}
	return nil
}

func writeWithContext(ctx context.Context, done <-chan struct{}, destination *os.File, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-done:
			return ErrInvalidInput
		default:
		}
		if err := destination.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			return err
		}
		written, err := destination.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return destination.SetWriteDeadline(time.Time{})
}

func (m *Manager) CloseStdin(ctx context.Context, id string) error {
	if ctx == nil {
		return ErrInvalidInput
	}
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil || current.state != StateRunning || current.stdinErr != nil {
		m.mu.Unlock()
		return ErrMissingExecution
	}
	gate, done := current.writeGate, current.done
	input := current.stdin
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrMissingExecution
	case <-gate:
	}
	defer func() { gate <- struct{}{} }()
	err := input.Close()
	m.mu.Lock()
	if live := m.sessions[id]; live != nil {
		live.stdinErr = os.ErrClosed
	}
	m.mu.Unlock()
	if err != nil {
		return commandError(err)
	}
	return nil
}

func (m *Manager) Interrupt(id string) error {
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil || current.state != StateRunning || current.leaderExited {
		m.mu.Unlock()
		return ErrMissingExecution
	}
	pid := current.command.Process.Pid
	err := syscall.Kill(-pid, syscall.SIGINT)
	m.mu.Unlock()
	if err != nil {
		return commandError(err)
	}
	return nil
}

func (m *Manager) Cancel(ctx context.Context, id string) error {
	if ctx == nil {
		return ErrMissingExecution
	}
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil {
		m.mu.Unlock()
		return ErrMissingExecution
	}
	if current.leaderExited && current.cleanupFinished && !current.cleanupOK {
		if !current.groupPinned {
			m.mu.Unlock()
			return ErrCleanupUnconfirmed
		}
		pid := current.command.Process.Pid
		m.mu.Unlock()
		if !stopGroup(pid) {
			return ErrCleanupUnconfirmed
		}
		m.mu.Lock()
		if live := m.sessions[id]; live != nil {
			live.cleanupOK = true
		}
		m.mu.Unlock()
		return nil
	}
	if current.state == StateRunning && !current.leaderExited {
		if current.requested == "" {
			current.requested = StateCanceled
		}
	}
	done := current.done
	m.mu.Unlock()
	m.terminate(id, StateCanceled)
	timer := time.NewTimer(processCancelGrace)
	select {
	case <-done:
		timer.Stop()
		return m.confirmCleanup(id)
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
	}
	m.killGroup(id, syscall.SIGKILL)
	timer.Reset(processCleanupTimeout - processCancelGrace)
	select {
	case <-done:
		timer.Stop()
		return m.confirmCleanup(id)
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return ErrCleanupUnconfirmed
	}
}

func (m *Manager) CloseAll(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	m.mu.Lock()
	m.closed = true
	ids := make([]string, 0, len(m.sessions))
	for id, current := range m.sessions {
		if current.state == StateRunning && !current.leaderExited && current.requested == "" {
			current.requested = StateCanceled
		}
		ids = append(ids, id)
	}
	m.mu.Unlock()
	cancelOK := true
	for _, id := range ids {
		if err := m.Cancel(ctx, id); err != nil && !errors.Is(err, ErrMissingExecution) {
			cancelOK = false
		}
	}
	for _, id := range ids {
		m.mu.Lock()
		current := m.sessions[id]
		var done <-chan struct{}
		if current != nil {
			done = current.done
		}
		m.mu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return false
		}
	}
	return cancelOK
}

func (m *Manager) Diagnostics() Diagnostics {
	m.mu.Lock()
	defer m.mu.Unlock()
	value := Diagnostics{OutputGapsTotal: m.outputGaps}
	for _, current := range m.sessions {
		if current.state == StateRunning || !current.cleanupOK {
			value.ActiveProcesses++
		}
		value.BufferedOutputBytes += current.buffered
	}
	return value
}

func (m *Manager) terminate(id string, state State) {
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil || current.leaderExited {
		m.mu.Unlock()
		return
	}
	if current.requested == "" {
		current.requested = state
	}
	pid := current.command.Process.Pid
	input := current.stdin
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	m.mu.Unlock()
	_ = input.Close()
	time.AfterFunc(processCancelGrace, func() { m.killGroup(id, syscall.SIGKILL) })
}

func (m *Manager) killGroup(id string, signal syscall.Signal) {
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil || current.cleanupOK || current.leaderExited {
		m.mu.Unlock()
		return
	}
	pid := current.command.Process.Pid
	_ = syscall.Kill(-pid, signal)
	m.mu.Unlock()
}

func (m *Manager) confirmCleanup(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[id]
	if current == nil || !current.cleanupOK {
		return ErrCleanupUnconfirmed
	}
	return nil
}

func (m *Manager) waitLeader(id string, command *exec.Cmd) {
	pid := command.Process.Pid
	waitErr := m.waitUnreaped(pid)
	if waitErr != nil {
		m.mu.Lock()
		if current := m.sessions[id]; current != nil {
			current.leaderExited = true
		}
		m.mu.Unlock()
		_ = command.Process.Kill()
		err := command.Wait()
		m.recordUnpinnedExit(id, err)
		return
	}
	m.mu.Lock()
	current := m.sessions[id]
	if current == nil {
		m.mu.Unlock()
		return
	}
	current.leaderExited = true
	current.groupPinned = true
	m.mu.Unlock()

	for !stopGroup(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	err := command.Wait()
	status, hasStatus := exitStatus(err)
	m.mu.Lock()
	current = m.sessions[id]
	if current == nil {
		m.mu.Unlock()
		return
	}
	if current.requested != "" {
		current.terminal = current.requested
	} else if !hasStatus || status.Signaled() {
		current.terminal = StateCanceled
	} else {
		current.terminal = StateExited
		code := status.ExitStatus()
		current.exitCode = &code
	}
	if hasStatus && status.Signaled() {
		signal := int(status.Signal())
		current.signal = &signal
	}
	current.cleanupFinished = true
	current.cleanupOK = true
	m.scheduleReaderDrainLocked(current)
	m.finishIfReadyLocked(current)
	m.mu.Unlock()
}

func (m *Manager) recordUnpinnedExit(id string, err error) {
	status, hasStatus := exitStatus(err)
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[id]
	if current == nil {
		return
	}
	current.leaderExited = true
	current.cleanupFinished = true
	current.cleanupOK = false
	if current.requested != "" {
		current.terminal = current.requested
	} else if !hasStatus || status.Signaled() {
		current.terminal = StateCanceled
	} else {
		current.terminal = StateExited
		code := status.ExitStatus()
		current.exitCode = &code
	}
	if hasStatus && status.Signaled() {
		signal := int(status.Signal())
		current.signal = &signal
	}
	m.scheduleReaderDrainLocked(current)
	m.finishIfReadyLocked(current)
}

func (m *Manager) scheduleReaderDrainLocked(current *session) {
	if current.readers == 0 {
		return
	}
	id := current.id
	readers := []io.ReadCloser{current.stdout, current.stderr}
	time.AfterFunc(processReaderDrainPeriod, func() {
		m.mu.Lock()
		live := m.sessions[id]
		stillReading := live != nil && live.readers > 0
		if stillReading {
			live.incomplete = true
		}
		m.mu.Unlock()
		if !stillReading {
			return
		}
		for _, reader := range readers {
			_ = reader.Close()
		}
	})
}

func waitUnreaped(pid int) error {
	var info [128]byte
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), uintptr(syscall.WEXITED|syscall.WNOWAIT), 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

func (m *Manager) readOutput(id, stream string, reader io.ReadCloser) {
	defer func() {
		_ = reader.Close()
		m.readerDone(id)
	}()
	buffer := make([]byte, processOutputChunkBytes)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			m.appendOutput(id, stream, buffer[:n])
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) appendOutput(id, stream string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[id]
	if current == nil || len(data) == 0 {
		return
	}
	current.sequence++
	chunk := Chunk{Sequence: current.sequence, Stream: stream, Data: append([]byte(nil), data...)}
	current.chunks = append(current.chunks, chunk)
	current.buffered += len(data)
	dropped := false
	for current.buffered > MaxBufferedBytesPerRun && len(current.chunks) > 0 {
		current.buffered -= len(current.chunks[0].Data)
		current.chunks = current.chunks[1:]
		dropped = true
	}
	if dropped {
		m.outputGaps++
	}
}

func (m *Manager) readerDone(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.sessions[id]; current != nil {
		current.readers--
		m.finishIfReadyLocked(current)
	}
}

func (m *Manager) finishIfReadyLocked(current *session) {
	if !current.leaderExited || !current.cleanupFinished || current.readers > 0 {
		return
	}
	current.state = current.terminal
	current.doneOnce.Do(func() {
		if current.timer != nil {
			current.timer.Stop()
		}
		close(current.done)
	})
}

func (m *Manager) activeLocked() int {
	active := 0
	for _, current := range m.sessions {
		if current.state == StateRunning || !current.cleanupOK {
			active++
		}
	}
	return active
}

func (m *Manager) pruneLocked() {
	if len(m.sessions) < MaxRetainedSessions {
		return
	}
	completed := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		if current.state != StateRunning && current.leaderExited && current.cleanupFinished && current.cleanupOK && current.readers == 0 {
			completed = append(completed, current)
		}
	}
	for _, current := range completed {
		if len(m.sessions) < MaxRetainedSessions {
			break
		}
		delete(m.sessions, current.id)
	}
}

func (s *session) read(cursor uint64) Read {
	earliest := s.sequence + 1
	if len(s.chunks) > 0 {
		earliest = s.chunks[0].Sequence
	}
	gap := s.incomplete || cursor < earliest-1
	selected := make([]Chunk, 0)
	bytes := 0
	next := cursor
	for _, chunk := range s.chunks {
		if chunk.Sequence <= cursor {
			continue
		}
		if bytes+len(chunk.Data) > MaxReadBytes {
			break
		}
		selected = append(selected, chunk)
		bytes += len(chunk.Data)
		next = chunk.Sequence
	}
	return Read{ExecutionID: s.id, Chunks: selected, NextCursor: next, Earliest: earliest, OutputGap: gap,
		State: s.state, ExitCode: s.exitCode, Signal: s.signal}
}

func validateShell(shell string) error {
	if !filepath.IsAbs(shell) {
		return ErrInvalidCommand
	}
	switch filepath.Base(shell) {
	case "bash", "zsh", "sh", "dash":
	default:
		return ErrInvalidCommand
	}
	info, err := os.Stat(shell)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ErrInvalidCommand
	}
	return nil
}

func validateDirectory(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return ErrPermissionDenied
		}
		return ErrInvalidWorkingDirectory
	}
	if !info.IsDir() {
		return ErrInvalidWorkingDirectory
	}
	if err := syscall.Access(directory, 1); err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
			return ErrPermissionDenied
		}
		return ErrInvalidWorkingDirectory
	}
	return nil
}

func childEnvironment(environ []string) []string {
	allowed := map[string]struct{}{"PATH": {}, "HOME": {}, "USER": {}, "LOGNAME": {}, "SHELL": {}, "TMPDIR": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}}
	result := make([]string, 0, len(allowed))
	for _, item := range environ {
		name, _, ok := strings.Cut(item, "=")
		if _, allowed := allowed[name]; ok && allowed {
			result = append(result, item)
		}
	}
	return result
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	value := hex.EncodeToString(data[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", value[:8], value[8:12], value[12:16], value[16:20], value[20:]), nil
}

func commandError(err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return ErrPermissionDenied
	}
	return err
}

func exitStatus(err error) (syscall.WaitStatus, bool) {
	if err == nil {
		return 0, true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return status, ok
}

func stopGroup(group int) bool {
	_ = syscall.Kill(-group, syscall.SIGKILL)
	return waitForGroupExit(group, liveProcessInGroup)
}

func waitForGroupExit(group int, inspect func(int) (bool, bool)) bool {
	deadline := time.Now().Add(processCleanupTimeout)
	for time.Now().Before(deadline) {
		live, verified := inspect(group)
		if !verified {
			return false
		}
		if !live {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	live, verified := inspect(group)
	return verified && !live
}

func liveProcessInGroup(group int) (bool, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		killErr := syscall.Kill(-group, 0)
		if killErr == nil {
			return true, false
		}
		if errors.Is(killErr, syscall.EPERM) {
			return true, false
		}
		if errors.Is(killErr, syscall.ESRCH) {
			return false, true
		}
		return true, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return true, false
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return true, false
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 3 {
			return true, false
		}
		if fields[0] == "Z" || fields[0] == "X" {
			continue
		}
		processGroup, err := strconv.Atoi(fields[2])
		if err == nil && processGroup == group {
			return true, true
		}
	}
	return false, true
}
