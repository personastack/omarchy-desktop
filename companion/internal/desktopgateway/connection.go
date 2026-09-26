package desktopgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/personastack/omarchy-desktop/companion/internal/wirejson"
	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const (
	maxInFlightCommands      = 32
	reservedRevocationSlots  = 1
	defaultHeartbeatInterval = 15 * time.Second
	minimumHeartbeatInterval = time.Millisecond
	maximumHeartbeatInterval = 30 * time.Second
	defaultWriteTimeout      = 15 * time.Second
	minimumWriteTimeout      = time.Millisecond
	maximumWriteTimeout      = 30 * time.Second
	maximumHandshakeWait     = 15 * time.Second
	heartbeatTimeout         = 45 * time.Second
)

var (
	ErrUnavailable      = errors.New("desktop gateway unavailable")
	ErrAlreadyConnected = errors.New("desktop gateway already connected")
	ErrInvalidFrame     = errors.New("desktop gateway frame invalid")
	ErrUpgradeRequired  = errors.New("desktop gateway protocol upgrade required")
	ErrConnectionEnded  = errors.New("desktop gateway connection ended")
	ErrCommandCapacity  = errors.New("desktop gateway command capacity reached")
	errSocketWrite      = errors.New("desktop gateway socket write failed")
)

type CommandHandler func(context.Context, agentgatewayruntime.DesktopControlFrame, func(agentgatewayruntime.DesktopControlFrame) error) agentgatewayruntime.DesktopControlFrame

type DiagnosticsProvider func() *agentgatewayruntime.DesktopControlDiagnostics

type Socket interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Close() error
}

type DialFunc func(context.Context, string, http.Header) (Socket, *http.Response, error)

type Options struct {
	Dialer            *websocket.Dialer
	Dial              DialFunc
	HeartbeatInterval time.Duration
	WriteTimeout      time.Duration
	Diagnostics       DiagnosticsProvider
	Now               func() time.Time
}

type Status struct {
	Connected       bool
	Readiness       string
	LastHeartbeatAt time.Time
}

type Connection struct {
	installation desktopcontrol.Installation
	handler      CommandHandler
	dialer       *websocket.Dialer
	dial         DialFunc
	interval     time.Duration
	writeTimeout time.Duration
	diagnostics  DiagnosticsProvider
	now          func() time.Time

	mu                   sync.Mutex
	writeMu              sync.Mutex
	conn                 Socket
	cancel               context.CancelFunc
	connecting           bool
	closing              bool
	connectCancel        context.CancelFunc
	connectingSocket     Socket
	done                 chan struct{}
	finishOnce           *sync.Once
	generation           uint64
	connected            bool
	currentStatus        Status
	lastError            error
	diagnosticsSupported bool
	activeCommands       map[string]context.CancelFunc
}

func New(installation desktopcontrol.Installation, origin string, handler CommandHandler, options Options) (*Connection, error) {
	if handler == nil || installation.Validate(origin) != nil {
		return nil, ErrUnavailable
	}
	if options.Dialer == nil {
		options.Dialer = websocket.DefaultDialer
	}
	if options.HeartbeatInterval == 0 {
		options.HeartbeatInterval = defaultHeartbeatInterval
	}
	if options.HeartbeatInterval < minimumHeartbeatInterval || options.HeartbeatInterval > maximumHeartbeatInterval {
		return nil, ErrUnavailable
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = defaultWriteTimeout
	}
	if options.WriteTimeout < minimumWriteTimeout || options.WriteTimeout > maximumWriteTimeout {
		return nil, ErrUnavailable
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Connection{
		installation:   installation,
		handler:        handler,
		dialer:         options.Dialer,
		dial:           options.Dial,
		interval:       options.HeartbeatInterval,
		writeTimeout:   options.WriteTimeout,
		diagnostics:    options.Diagnostics,
		now:            options.Now,
		activeCommands: make(map[string]context.CancelFunc),
	}, nil
}

func (c *Connection) Connect(ctx context.Context) error {
	return c.ConnectWithLifetime(ctx, ctx)
}

// ConnectWithLifetime uses ctx for the bounded handshake and lifetime for the
// connected socket. This lets request cancellation stop setup without owning
// a successfully established desktop session.
func (c *Connection) ConnectWithLifetime(ctx, lifetime context.Context) error {
	if ctx == nil || lifetime == nil {
		return ErrUnavailable
	}
	if err := lifetime.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return ErrConnectionEnded
	}
	if c.connected || c.conn != nil || c.connecting {
		c.mu.Unlock()
		return ErrAlreadyConnected
	}
	c.connecting = true
	defer func() {
		c.mu.Lock()
		c.connecting = false
		c.connectCancel = nil
		c.connectingSocket = nil
		c.mu.Unlock()
	}()

	dialer := *c.dialer
	header := make(http.Header)
	header.Set("X-Desktop-Control-Installation-ID", c.installation.InstallationID)
	header.Set("Authorization", "Bearer "+c.installation.MachineCredential)
	dialContext, cancel := context.WithTimeout(ctx, maximumHandshakeWait)
	c.connectCancel = cancel
	c.mu.Unlock()
	defer cancel()
	var conn Socket
	var response *http.Response
	var err error
	if c.dial != nil {
		conn, response, err = c.dial(dialContext, c.installation.GatewayWebsocketURL, header)
	} else {
		conn, response, err = dialer.DialContext(dialContext, c.installation.GatewayWebsocketURL, header)
	}
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return ErrUnavailable
		}
		return fmt.Errorf("connect desktop gateway: %w", err)
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		_ = conn.Close()
		return ErrConnectionEnded
	}
	c.connectingSocket = conn
	c.mu.Unlock()
	var handshakeMu sync.Mutex
	handshakeComplete := false
	stopHandshakeCancel := context.AfterFunc(ctx, func() {
		handshakeMu.Lock()
		defer handshakeMu.Unlock()
		if !handshakeComplete {
			_ = conn.Close()
		}
	})
	defer stopHandshakeCancel()
	conn.SetReadLimit(agentgatewayruntime.DesktopControlFrameLimit)
	if err := conn.SetReadDeadline(c.now().Add(maximumHandshakeWait)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set desktop gateway handshake deadline: %w", err)
	}
	frame, err := readFrame(conn)
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}
	if frame.Type == agentgatewayruntime.DesktopControlFrameFailure && frame.ErrorCode == "upgrade_required" {
		_ = conn.Close()
		return ErrUpgradeRequired
	}
	if frame.Version != agentgatewayruntime.DesktopControlProtocolVersion {
		_ = conn.Close()
		return ErrUpgradeRequired
	}
	if frame.Type != agentgatewayruntime.DesktopControlFrameReady || agentgatewayruntime.ValidateDesktopControlFrame(frame) != nil {
		_ = conn.Close()
		return ErrInvalidFrame
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return fmt.Errorf("clear desktop gateway handshake deadline: %w", err)
	}
	if err := conn.SetReadDeadline(c.now().Add(heartbeatTimeout)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set desktop gateway heartbeat deadline: %w", err)
	}
	if err := lifetime.Err(); err != nil {
		_ = conn.Close()
		return err
	}
	connectionContext, connectionCancel := context.WithCancel(lifetime)
	handshakeMu.Lock()
	if err := ctx.Err(); err != nil {
		handshakeMu.Unlock()
		connectionCancel()
		_ = conn.Close()
		return err
	}
	c.mu.Lock()
	if c.closing || c.connected || c.conn != nil {
		c.mu.Unlock()
		handshakeMu.Unlock()
		connectionCancel()
		_ = conn.Close()
		return ErrConnectionEnded
	}
	c.generation++
	generation := c.generation
	c.conn = conn
	c.connectingSocket = nil
	c.cancel = connectionCancel
	c.done = make(chan struct{})
	c.finishOnce = &sync.Once{}
	c.connected = true
	c.connecting = false
	c.currentStatus = Status{Connected: true, Readiness: "unknown"}
	c.currentStatus.LastHeartbeatAt = frame.LastHeartbeat
	c.lastError = nil
	c.diagnosticsSupported = frame.DiagnosticsSupported
	c.activeCommands = make(map[string]context.CancelFunc)
	c.mu.Unlock()
	handshakeComplete = true
	handshakeMu.Unlock()
	go c.readLoop(connectionContext, generation, conn)
	go c.heartbeatLoop(connectionContext, generation)
	return nil
}

func (c *Connection) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentStatus
}

func (c *Connection) SetReadiness(value string) error {
	if !agentgatewayruntime.IsDesktopControlReadiness(value) {
		return ErrInvalidFrame
	}
	c.mu.Lock()
	c.currentStatus.Readiness = value
	generation := c.generation
	connected := c.connected
	c.mu.Unlock()
	if !connected {
		return nil
	}
	if err := c.write(generation, c.heartbeatFrame(value)); err != nil {
		c.finish(generation, err)
		return err
	}
	return nil
}

func (c *Connection) Wait(ctx context.Context) error {
	if ctx == nil {
		return ErrConnectionEnded
	}
	c.mu.Lock()
	done := c.done
	connected := c.connected
	c.mu.Unlock()
	if done == nil {
		return ErrConnectionEnded
	}
	if !connected {
		select {
		case <-done:
			return ErrConnectionEnded
		default:
		}
	}
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.lastError != nil {
			return c.lastError
		}
		return ErrConnectionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Connection) Close() {
	c.mu.Lock()
	c.closing = true
	cancel := c.connectCancel
	connectingSocket := c.connectingSocket
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if connectingSocket != nil {
		_ = connectingSocket.Close()
	}
	c.finish(0, context.Canceled)
}

func (c *Connection) readLoop(ctx context.Context, generation uint64, conn Socket) {
	for {
		if !c.isConnected(generation) {
			return
		}
		frame, err := readFrame(conn)
		if !c.isConnected(generation) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				c.finish(generation, err)
			}
			return
		}
		if frame.Version != agentgatewayruntime.DesktopControlProtocolVersion {
			c.finish(generation, ErrUpgradeRequired)
			return
		}
		if err := agentgatewayruntime.ValidateDesktopControlFrame(frame); err != nil {
			c.finish(generation, ErrInvalidFrame)
			return
		}
		switch frame.Type {
		case agentgatewayruntime.DesktopControlFrameHeartbeat:
			if err := conn.SetReadDeadline(c.now().Add(heartbeatTimeout)); err != nil {
				c.finish(generation, err)
				return
			}
			c.recordHeartbeat(generation, frame.LastHeartbeat)
		case agentgatewayruntime.DesktopControlFrameFailure:
			if frame.ErrorCode == "upgrade_required" {
				c.finish(generation, ErrUpgradeRequired)
				return
			}
			c.finish(generation, ErrInvalidFrame)
			return
		case agentgatewayruntime.DesktopControlFrameCommand:
			if err := c.admitCommand(ctx, generation, frame); err != nil {
				if errors.Is(err, ErrCommandCapacity) {
					if writeErr := c.write(generation, failureFrame(frame.RequestID, "desktop_command_capacity", "The desktop is handling other commands. Retry after one finishes.")); writeErr != nil {
						c.finish(generation, writeErr)
						return
					}
					continue
				}
				c.finish(generation, err)
				return
			}
		default:
			c.finish(generation, ErrInvalidFrame)
			return
		}
	}
}

func (c *Connection) isConnected(generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected && c.generation == generation
}

func (c *Connection) heartbeatLoop(ctx context.Context, generation uint64) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.finish(generation, ctx.Err())
			return
		case <-ticker.C:
			frame := c.heartbeatFrame(c.Status().Readiness)
			if err := c.write(generation, frame); err != nil {
				c.finish(generation, err)
				return
			}
			c.mu.Lock()
			if generation == c.generation && c.connected {
				c.currentStatus.Readiness = frame.Readiness
			}
			c.mu.Unlock()
		}
	}
}

func (c *Connection) admitCommand(ctx context.Context, generation uint64, frame agentgatewayruntime.DesktopControlFrame) error {
	if frame.Target == nil || frame.Target.InstallationID != c.installation.InstallationID || frame.RequestID == "" {
		return ErrInvalidFrame
	}
	if !frame.DeadlineAt.After(c.now()) {
		return c.write(generation, failureFrame(frame.RequestID, "desktop_command_deadline", "The desktop command deadline has expired."))
	}
	c.mu.Lock()
	if generation != c.generation || !c.connected {
		c.mu.Unlock()
		return ErrConnectionEnded
	}
	if _, exists := c.activeCommands[frame.RequestID]; exists {
		c.mu.Unlock()
		return ErrInvalidFrame
	}
	activeCount := len(c.activeCommands)
	if !hasCapacity(frame.Operation, activeCount) {
		c.mu.Unlock()
		return ErrCommandCapacity
	}
	commandContext, cancel := context.WithDeadline(ctx, frame.DeadlineAt)
	c.activeCommands[frame.RequestID] = cancel
	c.mu.Unlock()
	go c.execute(commandContext, generation, frame, cancel)
	return nil
}

func (c *Connection) execute(ctx context.Context, generation uint64, frame agentgatewayruntime.DesktopControlFrame, cancel context.CancelFunc) {
	requestID := frame.RequestID
	defer func() {
		cancel()
		c.mu.Lock()
		if generation == c.generation {
			delete(c.activeCommands, requestID)
		}
		c.mu.Unlock()
	}()
	emit := func(chunk agentgatewayruntime.DesktopControlFrame) error {
		if chunk.Type != agentgatewayruntime.DesktopControlFrameResultChunk || chunk.RequestID != requestID || agentgatewayruntime.ValidateDesktopControlFrame(chunk) != nil {
			return ErrInvalidFrame
		}
		err := c.writeContext(ctx, generation, chunk)
		if errors.Is(err, errSocketWrite) {
			c.finish(generation, err)
		}
		return err
	}
	response := c.handler(ctx, frame, emit)
	if ctx.Err() != nil {
		return
	}
	if response.RequestID != requestID || (response.Type != agentgatewayruntime.DesktopControlFrameResult && response.Type != agentgatewayruntime.DesktopControlFrameFailure) || response.Version != agentgatewayruntime.DesktopControlProtocolVersion || agentgatewayruntime.ValidateDesktopControlFrame(response) != nil {
		response = failureFrame(requestID, "desktop_response_invalid", "The local desktop runtime returned an invalid response.")
	}
	if err := c.writeContext(ctx, generation, response); err != nil {
		if ctx.Err() != nil && !errors.Is(err, errSocketWrite) {
			return
		}
		c.finish(generation, err)
		return
	}
}

func (c *Connection) write(generation uint64, frame agentgatewayruntime.DesktopControlFrame) error {
	return c.writeContext(nil, generation, frame)
}

func (c *Connection) writeContext(ctx context.Context, generation uint64, frame agentgatewayruntime.DesktopControlFrame) error {
	encoded, err := agentgatewayruntime.MarshalDesktopControlFrame(frame)
	if err != nil {
		return ErrInvalidFrame
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	connected := c.connected && generation == c.generation
	c.mu.Unlock()
	if !connected || conn == nil {
		return ErrConnectionEnded
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	deadline := c.now().Add(c.writeTimeout)
	if ctx != nil {
		if commandDeadline, ok := ctx.Deadline(); ok && commandDeadline.Before(deadline) {
			deadline = commandDeadline
		}
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("%w: set write deadline: %w", errSocketWrite, err)
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err := conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
		return fmt.Errorf("%w: write frame: %w", errSocketWrite, err)
	}
	return nil
}

func (c *Connection) finish(generation uint64, cause error) {
	c.mu.Lock()
	if c.finishOnce == nil || (generation != 0 && generation != c.generation) {
		c.mu.Unlock()
		return
	}
	once := c.finishOnce
	conn := c.conn
	cancel := c.cancel
	commands := make([]context.CancelFunc, 0, len(c.activeCommands))
	for _, stop := range c.activeCommands {
		commands = append(commands, stop)
	}
	done := c.done
	c.connected = false
	c.currentStatus.Connected = false
	c.mu.Unlock()
	once.Do(func() {
		c.mu.Lock()
		if generation == 0 || generation == c.generation {
			if cause != nil {
				c.lastError = cause
			}
		}
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		for _, stop := range commands {
			stop()
		}
		if conn != nil {
			_ = conn.Close()
		}
		c.mu.Lock()
		if generation == 0 || generation == c.generation {
			c.conn = nil
			c.cancel = nil
			c.activeCommands = make(map[string]context.CancelFunc)
		}
		c.mu.Unlock()
		if done != nil {
			close(done)
		}
	})
}

func (c *Connection) recordHeartbeat(generation uint64, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation == c.generation && c.connected {
		if at.IsZero() {
			at = c.now().UTC()
		}
		c.currentStatus.LastHeartbeatAt = at
	}
}

func readFrame(conn Socket) (agentgatewayruntime.DesktopControlFrame, error) {
	messageType, raw, err := conn.ReadMessage()
	if err != nil {
		return agentgatewayruntime.DesktopControlFrame{}, fmt.Errorf("read desktop gateway frame: %w", err)
	}
	if messageType != websocket.TextMessage || !wirejson.ValidUniqueJSON(raw) {
		return agentgatewayruntime.DesktopControlFrame{}, ErrInvalidFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var frame agentgatewayruntime.DesktopControlFrame
	if err := decoder.Decode(&frame); err != nil || decoder.Decode(new(any)) != io.EOF {
		return agentgatewayruntime.DesktopControlFrame{}, ErrInvalidFrame
	}
	return frame, nil
}

func safeReadiness(value string) string {
	if agentgatewayruntime.IsDesktopControlReadiness(value) {
		return value
	}
	return "unknown"
}

func (c *Connection) heartbeatFrame(readiness string) agentgatewayruntime.DesktopControlFrame {
	c.mu.Lock()
	diagnosticsSupported := c.diagnosticsSupported
	c.mu.Unlock()
	frame := agentgatewayruntime.DesktopControlFrame{
		Version:       agentgatewayruntime.DesktopControlProtocolVersion,
		Type:          agentgatewayruntime.DesktopControlFrameHeartbeat,
		LastHeartbeat: c.now().UTC(),
		Readiness:     safeReadiness(readiness),
	}
	if diagnosticsSupported && c.diagnostics != nil {
		frame.Diagnostics = c.diagnostics()
	}
	if agentgatewayruntime.ValidateDesktopControlFrame(frame) != nil {
		frame.Diagnostics = nil
	}
	return frame
}

func failureFrame(requestID, code, message string) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{
		Version:      agentgatewayruntime.DesktopControlProtocolVersion,
		Type:         agentgatewayruntime.DesktopControlFrameFailure,
		RequestID:    requestID,
		ErrorCode:    code,
		ErrorMessage: message,
	}
}

func hasCapacity(operation agentgatewayruntime.DesktopControlOperation, activeCount int) bool {
	if activeCount < 0 || activeCount >= maxInFlightCommands {
		return false
	}
	if operation == agentgatewayruntime.DesktopControlOperationRevokeConfig || operation == agentgatewayruntime.DesktopControlOperationRevokeBinding {
		return true
	}
	return activeCount < maxInFlightCommands-reservedRevocationSlots
}
