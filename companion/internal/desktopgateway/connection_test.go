package desktopgateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

func TestConnectionConnectsAndDispatchesCommand(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{
		Version:              agentgatewayruntime.DesktopControlProtocolVersion,
		Type:                 agentgatewayruntime.DesktopControlFrameReady,
		LastHeartbeat:        time.Now().UTC(),
		DiagnosticsSupported: true,
	})
	var handled atomic.Bool
	client, err := New(installation, installation.EnvironmentOrigin, func(_ context.Context, frame agentgatewayruntime.DesktopControlFrame, _ func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		handled.Store(true)
		return agentgatewayruntime.DesktopControlFrame{
			Version:   agentgatewayruntime.DesktopControlProtocolVersion,
			Type:      agentgatewayruntime.DesktopControlFrameResult,
			RequestID: frame.RequestID,
			Result:    json.RawMessage(`{"ok":true}`),
		}
	}, Options{
		Dial: socketDialer(socket, installation),
	})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if !client.Status().Connected {
		t.Fatal("gateway connection status is not connected")
	}
	if err := client.SetReadiness("ready"); err != nil {
		t.Fatalf("report readiness: %v", err)
	}
	heartbeat := readWrittenFrame(t, socket)
	if heartbeat.Type != agentgatewayruntime.DesktopControlFrameHeartbeat || heartbeat.Readiness != "ready" {
		t.Fatalf("unexpected readiness heartbeat: %#v", heartbeat)
	}
	if err := socket.send(commandFrame(installation.InstallationID, "request-1", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("send gateway command")
	}
	response := readWrittenFrame(t, socket)
	if response.RequestID != "request-1" || response.Type != agentgatewayruntime.DesktopControlFrameResult || string(response.Result) != `{"ok":true}` || !handled.Load() {
		t.Fatalf("unexpected gateway response: %#v", response)
	}
}

func TestConnectionRejectsExpiredCommandWithoutDispatch(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	var handled atomic.Bool
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		handled.Store(true)
		return agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: "expired"}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if err := socket.send(commandFrame(installation.InstallationID, "expired", time.Now().Add(-time.Second))); err != nil {
		t.Fatal("send expired gateway command")
	}
	response := readWrittenFrame(t, socket)
	if response.Type != agentgatewayruntime.DesktopControlFrameFailure || response.ErrorCode != "desktop_command_deadline" {
		t.Fatalf("expired command response = %#v", response)
	}
	if handled.Load() {
		t.Fatal("expired command reached the runtime")
	}
}

func TestConnectionDoesNotEmitChunkAfterCommandDeadline(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	chunkError := make(chan error, 1)
	client, err := New(installation, installation.EnvironmentOrigin, func(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, emit func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		<-ctx.Done()
		chunkError <- emit(agentgatewayruntime.DesktopControlFrame{
			Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameResultChunk,
			RequestID: frame.RequestID, StreamID: frame.RequestID, Sequence: 1, StreamChannel: "stdout", StreamData: []byte("late"),
		})
		return failureFrame(frame.RequestID, "late", "late")
	}, Options{Dial: socketDialer(socket, installation), HeartbeatInterval: maximumHeartbeatInterval})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if err := socket.send(commandFrame(installation.InstallationID, "late-chunk", time.Now().Add(50*time.Millisecond))); err != nil {
		t.Fatal("send deadline-bound command")
	}
	select {
	case err := <-chunkError:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("late chunk error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not attempt its late result chunk")
	}
	select {
	case message := <-socket.outgoing:
		t.Fatalf("gateway emitted after command deadline: %s", message.raw)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestConnectionSuppressesTerminalResponseExpiredWhileQueued(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	handlerReturned := make(chan struct{})
	client, err := New(installation, installation.EnvironmentOrigin, func(_ context.Context, frame agentgatewayruntime.DesktopControlFrame, _ func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		close(handlerReturned)
		return agentgatewayruntime.DesktopControlFrame{Version: agentgatewayruntime.DesktopControlProtocolVersion,
			Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: frame.RequestID, Result: json.RawMessage(`{"ok":true}`)}
	}, Options{Dial: socketDialer(socket, installation), HeartbeatInterval: maximumHeartbeatInterval})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	client.writeMu.Lock()
	deadline := time.Now().Add(60 * time.Millisecond)
	if err := socket.send(commandFrame(installation.InstallationID, "queued-terminal", deadline)); err != nil {
		client.writeMu.Unlock()
		t.Fatal("send terminal response command")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		client.writeMu.Unlock()
		t.Fatal("command handler did not return")
	}
	timer := time.NewTimer(time.Until(deadline) + 10*time.Millisecond)
	<-timer.C
	client.writeMu.Unlock()
	select {
	case message := <-socket.outgoing:
		t.Fatalf("gateway emitted a terminal response after expiry: %s", message.raw)
	case <-time.After(20 * time.Millisecond):
	}
	if !client.Status().Connected {
		t.Fatal("expired terminal response closed the healthy connection")
	}
}

func TestConnectionClosesOnSocketWriteTimeout(t *testing.T) {
	t.Parallel()
	for _, streamChunk := range []bool{false, true} {
		name := "terminal response"
		if streamChunk {
			name = "result chunk"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			installation := testInstallation()
			socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
			commandContext := make(chan context.Context, 1)
			writeAttempted := make(chan struct{}, 1)
			client, err := New(installation, installation.EnvironmentOrigin, func(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, emit func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
				commandContext <- ctx
				if streamChunk {
					writeAttempted <- struct{}{}
					_ = emit(agentgatewayruntime.DesktopControlFrame{
						Version: 1, Type: agentgatewayruntime.DesktopControlFrameResultChunk, RequestID: frame.RequestID,
						StreamID: frame.RequestID, Sequence: 1, StreamChannel: "stdout", StreamData: []byte("output"),
					})
				}
				return agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: frame.RequestID, Result: json.RawMessage(`{"ok":true}`)}
			}, Options{Dial: socketDialer(socket, installation), WriteTimeout: 20 * time.Millisecond})
			if err != nil {
				t.Fatal("create desktop gateway connection")
			}
			if err := client.Connect(context.Background()); err != nil {
				t.Fatalf("connect desktop gateway: %v", err)
			}
			t.Cleanup(client.Close)
			socket.setWritesStalled(true)
			if err := socket.send(commandFrame(installation.InstallationID, "stalled-write", time.Now().Add(time.Second))); err != nil {
				t.Fatal("send gateway command")
			}
			ctx := <-commandContext
			if streamChunk {
				select {
				case <-writeAttempted:
				case <-time.After(time.Second):
					t.Fatal("handler did not attempt its result chunk")
				}
			}
			select {
			case <-socket.closed:
			case <-time.After(time.Second):
				t.Fatal("socket write timeout did not close the connection")
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("socket write timeout did not cancel the active command")
			}
			if client.Status().Connected {
				t.Fatal("connection remained healthy after socket write timeout")
			}
		})
	}
}

func TestStaleReadLoopCannotConsumeCurrentConnectionFrame(t *testing.T) {
	t.Parallel()
	currentSocket := newUnreadyFakeSocket()
	var handled atomic.Bool
	client := &Connection{
		conn: currentSocket, connected: true, generation: 2,
		activeCommands: make(map[string]context.CancelFunc),
		handler: func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
			handled.Store(true)
			return agentgatewayruntime.DesktopControlFrame{}
		},
	}
	if err := currentSocket.send(commandFrame("installation_1", "current-request", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("queue current-generation command")
	}
	client.readLoop(context.Background(), 1, &singleMessageSocket{})
	if handled.Load() {
		t.Fatal("stale read loop dispatched a current connection command")
	}
	if len(currentSocket.incoming) != 1 {
		t.Fatal("stale read loop consumed a current connection frame")
	}
}

func TestHeartbeatFrameReadsDiagnosticsSupportSafely(t *testing.T) {
	t.Parallel()
	client, err := New(testInstallation(), "https://personastack.ericgreer.info", func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Diagnostics: func() *agentgatewayruntime.DesktopControlDiagnostics {
		return &agentgatewayruntime.DesktopControlDiagnostics{}
	}})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for index := range 1000 {
			client.mu.Lock()
			client.diagnosticsSupported = index%2 == 0
			client.mu.Unlock()
		}
	}()
	for range 1000 {
		frame := client.heartbeatFrame("ready")
		if frame.Type != agentgatewayruntime.DesktopControlFrameHeartbeat {
			t.Fatalf("heartbeat frame type = %q", frame.Type)
		}
	}
	writers.Wait()
}

func TestConnectionBoundsStalledSocketWrite(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	entered := make(chan struct{}, maxInFlightCommands)
	client, err := New(installation, installation.EnvironmentOrigin, func(ctx context.Context, frame agentgatewayruntime.DesktopControlFrame, _ func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		entered <- struct{}{}
		<-ctx.Done()
		return failureFrame(frame.RequestID, "cancelled", "cancelled")
	}, Options{Dial: socketDialer(socket, installation), HeartbeatInterval: maximumHeartbeatInterval, WriteTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	activeCommands := maxInFlightCommands - reservedRevocationSlots
	for index := range activeCommands {
		if err := socket.send(commandFrame(installation.InstallationID, fmt.Sprintf("active-%d", index), time.Now().Add(time.Minute))); err != nil {
			t.Fatal("send active command")
		}
	}
	for range activeCommands {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("active command was not admitted")
		}
	}
	socket.setWritesStalled(true)
	if err := socket.send(commandFrame(installation.InstallationID, "capacity", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("send over-capacity command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Wait(ctx); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("gateway close after bounded write error = %v", err)
	}
}

func TestConnectionFencesCommandForAnotherInstallation(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	var handled atomic.Bool
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		handled.Store(true)
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if err := socket.send(commandFrame("install_another_machine", "wrong-target", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("send wrong-installation command")
	}
	if err := client.Wait(context.Background()); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("wrong-installation command error = %v", err)
	}
	if handled.Load() {
		t.Fatal("wrong-installation command reached the runtime")
	}
}

func TestConnectionRejectsInvalidReadiness(t *testing.T) {
	t.Parallel()
	client, err := New(testInstallation(), "https://personastack.ericgreer.info", func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.SetReadiness("unknown-state"); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("invalid readiness error = %v", err)
	}
}

func TestHasCapacityReservesRevocationSlot(t *testing.T) {
	t.Parallel()
	if hasCapacity(agentgatewayruntime.DesktopControlOperationStatus, maxInFlightCommands-reservedRevocationSlots) {
		t.Fatal("ordinary command consumed the reserved revocation slot")
	}
	if !hasCapacity(agentgatewayruntime.DesktopControlOperationRevokeConfig, maxInFlightCommands-reservedRevocationSlots) {
		t.Fatal("config revocation could not use the reserved slot")
	}
	if !hasCapacity(agentgatewayruntime.DesktopControlOperationRevokeBinding, maxInFlightCommands-reservedRevocationSlots) {
		t.Fatal("binding revocation could not use the reserved slot")
	}
	if hasCapacity(agentgatewayruntime.DesktopControlOperationRevokeBinding, maxInFlightCommands) {
		t.Fatal("revocation exceeded total in-flight capacity")
	}
}

func TestReadFrameRejectsDuplicateAndUnknownFields(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"version":1,"version":1,"type":"ready"}`,
		`{"version":1,"type":"ready","unexpected":true}`,
	} {
		if _, err := readFrame(&singleMessageSocket{messageType: websocket.TextMessage, raw: []byte(raw)}); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("invalid frame error = %v for %s", err, raw)
		}
	}
}

func TestConnectionMapsInvalidHandlerResponseToFiniteFailure(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: "wrong-id"}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if err := socket.send(commandFrame(installation.InstallationID, "request-1", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("send gateway command")
	}
	response := readWrittenFrame(t, socket)
	if response.RequestID != "request-1" || response.Type != agentgatewayruntime.DesktopControlFrameFailure || response.ErrorCode != "desktop_response_invalid" {
		t.Fatalf("invalid handler response was not mapped: %#v", response)
	}
}

func TestConnectionForwardsValidatedChunksBeforeTerminalResponse(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	client, err := New(installation, installation.EnvironmentOrigin, func(_ context.Context, frame agentgatewayruntime.DesktopControlFrame, emit func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		chunk := agentgatewayruntime.DesktopControlFrame{
			Version:       1,
			Type:          agentgatewayruntime.DesktopControlFrameResultChunk,
			RequestID:     frame.RequestID,
			StreamID:      "process_1",
			Sequence:      1,
			StreamChannel: "stdout",
			StreamData:    []byte("output"),
		}
		if err := emit(chunk); err != nil {
			return failureFrame(frame.RequestID, "desktop_stream_failed", "The local desktop could not stream output.")
		}
		return agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameResult, RequestID: frame.RequestID}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect desktop gateway: %v", err)
	}
	t.Cleanup(client.Close)
	if err := socket.send(commandFrame(installation.InstallationID, "stream", time.Now().Add(time.Minute))); err != nil {
		t.Fatal("send gateway command")
	}
	chunk := readWrittenFrame(t, socket)
	terminal := readWrittenFrame(t, socket)
	if chunk.Type != agentgatewayruntime.DesktopControlFrameResultChunk || chunk.RequestID != "stream" || string(chunk.StreamData) != "output" {
		t.Fatalf("invalid streamed chunk: %#v", chunk)
	}
	if terminal.Type != agentgatewayruntime.DesktopControlFrameResult || terminal.RequestID != "stream" {
		t.Fatalf("invalid terminal frame: %#v", terminal)
	}
}

func TestConnectionClosesWhenItsOwnerContextEnds(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newFakeSocket(t, agentgatewayruntime.DesktopControlFrame{Version: 1, Type: agentgatewayruntime.DesktopControlFrameReady})
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	ownerContext, cancel := context.WithCancel(context.Background())
	if err := client.Connect(ownerContext); err != nil {
		cancel()
		t.Fatalf("connect desktop gateway: %v", err)
	}
	cancel()
	if err := client.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation result = %v", err)
	}
	if client.Status().Connected {
		t.Fatal("gateway remained connected after its owner stopped")
	}
}

func TestConnectionCloseCancelsInProgressDial(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	dialStarted := make(chan struct{})
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Dial: func(ctx context.Context, _ string, _ http.Header) (Socket, *http.Response, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	connected := make(chan error, 1)
	go func() { connected <- client.Connect(context.Background()) }()
	<-dialStarted
	client.Close()
	if err := <-connected; !errors.Is(err, context.Canceled) {
		t.Fatalf("connect result after close = %v", err)
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrConnectionEnded) {
		t.Fatalf("reconnect after permanent close = %v", err)
	}
}

func TestConnectionCloseInterruptsReadyHandshake(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := newUnreadyFakeSocket()
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	connected := make(chan error, 1)
	go func() { connected <- client.Connect(context.Background()) }()
	<-socket.readStarted
	client.Close()
	if err := <-connected; err == nil {
		t.Fatal("handshake succeeded after permanent close")
	}
	select {
	case <-socket.closed:
	default:
		t.Fatal("close did not interrupt the handshake socket")
	}
}

func TestConnectionHeartbeatIntervalStaysBelowGatewayTimeout(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	handler := func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}
	if _, err := New(installation, installation.EnvironmentOrigin, handler, Options{HeartbeatInterval: maximumHeartbeatInterval}); err != nil {
		t.Fatalf("maximum heartbeat interval rejected: %v", err)
	}
	if _, err := New(installation, installation.EnvironmentOrigin, handler, Options{HeartbeatInterval: maximumHeartbeatInterval + time.Second}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("heartbeat interval above the timeout accepted: %v", err)
	}
}

func TestConnectionRequiresProducerSupportedProtocolVersion(t *testing.T) {
	t.Parallel()
	installation := testInstallation()
	socket := &fakeSocket{incoming: make(chan socketMessage, 1), outgoing: make(chan socketMessage, 1), closed: make(chan struct{})}
	socket.incoming <- socketMessage{messageType: websocket.TextMessage, raw: []byte(`{"version":2,"type":"ready"}`)}
	client, err := New(installation, installation.EnvironmentOrigin, func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame {
		return agentgatewayruntime.DesktopControlFrame{}
	}, Options{Dial: socketDialer(socket, installation)})
	if err != nil {
		t.Fatal("create desktop gateway connection")
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("protocol version error = %v", err)
	}
}

func testInstallation() desktopcontrol.Installation {
	return desktopcontrol.Installation{
		EnvironmentOrigin:   "https://personastack.ericgreer.info",
		InstallationID:      "install_linux_test",
		MachineCredential:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		GatewayWebsocketURL: "ws://cluster-agent.personastack.lan/v1/desktop-control/ws",
	}
}

func socketDialer(socket Socket, installation desktopcontrol.Installation) DialFunc {
	return func(_ context.Context, url string, header http.Header) (Socket, *http.Response, error) {
		if url != installation.GatewayWebsocketURL || header.Get("X-Desktop-Control-Installation-ID") != installation.InstallationID || header.Get("Authorization") != "Bearer "+installation.MachineCredential {
			return nil, nil, ErrUnavailable
		}
		return socket, nil, nil
	}
}

func commandFrame(installationID, requestID string, deadline time.Time) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{
		Version:   agentgatewayruntime.DesktopControlProtocolVersion,
		Type:      agentgatewayruntime.DesktopControlFrameCommand,
		RequestID: requestID,
		Target: &agentgatewayruntime.DesktopControlTarget{
			InstallationID: installationID,
			WorkspaceID:    "workspace_1",
			ConfigID:       "config_1",
			PersonaID:      "persona_1",
			RunID:          "run_1",
			Generation:     1,
		},
		Operation:  agentgatewayruntime.DesktopControlOperationStatus,
		Arguments:  json.RawMessage(`{"request":"status"}`),
		DeadlineAt: deadline.UTC(),
	}
}

func newFakeSocket(t *testing.T, ready agentgatewayruntime.DesktopControlFrame) *fakeSocket {
	t.Helper()
	socket := newUnreadyFakeSocket()
	if err := socket.send(ready); err != nil {
		t.Fatal("queue ready frame")
	}
	return socket
}

func newUnreadyFakeSocket() *fakeSocket {
	return &fakeSocket{
		incoming: make(chan socketMessage, 8), outgoing: make(chan socketMessage, 8),
		closed: make(chan struct{}), readStarted: make(chan struct{}),
	}
}

func readWrittenFrame(t *testing.T, socket *fakeSocket) agentgatewayruntime.DesktopControlFrame {
	t.Helper()
	message := <-socket.outgoing
	if message.messageType != websocket.TextMessage {
		t.Fatalf("written websocket message type = %d", message.messageType)
	}
	var frame agentgatewayruntime.DesktopControlFrame
	if err := json.Unmarshal(message.raw, &frame); err != nil {
		t.Fatalf("decode written websocket frame: %v", err)
	}
	return frame
}

type socketMessage struct {
	messageType int
	raw         []byte
}

type fakeSocket struct {
	incoming      chan socketMessage
	outgoing      chan socketMessage
	closed        chan struct{}
	close         sync.Once
	readOnce      sync.Once
	readStarted   chan struct{}
	writeStateMu  sync.Mutex
	writeDeadline time.Time
	writesStalled bool
}

func (s *fakeSocket) ReadMessage() (int, []byte, error) {
	s.readOnce.Do(func() {
		if s.readStarted != nil {
			close(s.readStarted)
		}
	})
	select {
	case message := <-s.incoming:
		return message.messageType, message.raw, nil
	case <-s.closed:
		return 0, nil, io.EOF
	}
}

func (s *fakeSocket) WriteMessage(messageType int, raw []byte) error {
	s.writeStateMu.Lock()
	deadline := s.writeDeadline
	stalled := s.writesStalled
	s.writeStateMu.Unlock()
	if stalled {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-timer.C:
			return os.ErrDeadlineExceeded
		case <-s.closed:
			return io.EOF
		}
	}
	select {
	case s.outgoing <- socketMessage{messageType: messageType, raw: append([]byte(nil), raw...)}:
		return nil
	case <-s.closed:
		return io.EOF
	}
}

func (s *fakeSocket) SetReadLimit(int64) {}

func (s *fakeSocket) SetReadDeadline(time.Time) error { return nil }

func (s *fakeSocket) SetWriteDeadline(deadline time.Time) error {
	s.writeStateMu.Lock()
	s.writeDeadline = deadline
	s.writeStateMu.Unlock()
	return nil
}

func (s *fakeSocket) setWritesStalled(stalled bool) {
	s.writeStateMu.Lock()
	s.writesStalled = stalled
	s.writeStateMu.Unlock()
}

func (s *fakeSocket) Close() error {
	s.close.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeSocket) send(frame agentgatewayruntime.DesktopControlFrame) error {
	encoded, err := agentgatewayruntime.MarshalDesktopControlFrame(frame)
	if err != nil {
		return err
	}
	s.incoming <- socketMessage{messageType: websocket.TextMessage, raw: encoded}
	return nil
}

type singleMessageSocket struct {
	messageType int
	raw         []byte
}

func (s *singleMessageSocket) ReadMessage() (int, []byte, error) { return s.messageType, s.raw, nil }

func (*singleMessageSocket) WriteMessage(int, []byte) error { return nil }

func (*singleMessageSocket) SetReadLimit(int64) {}

func (*singleMessageSocket) SetReadDeadline(time.Time) error { return nil }

func (*singleMessageSocket) SetWriteDeadline(time.Time) error { return nil }

func (*singleMessageSocket) Close() error { return nil }
