package proxy

import (
	"bufio"
	"context"
	"errors"
	"go.minekube.com/gate/pkg/config"
	"go.minekube.com/gate/pkg/proto"
	"go.minekube.com/gate/pkg/proto/codec"
	"go.minekube.com/gate/pkg/proto/packet"
	"go.minekube.com/gate/pkg/proto/state"
	"go.minekube.com/gate/pkg/util/errs"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sessionHandler handles received packets from the associated connection.
//
// Since connections transition between states packets need to be handled differently,
// this behaviour is divided between sessions by sessionHandlers.
type sessionHandler interface {
	handlePacket(ctx context.Context, p proto.Packet)                // Called to handle incoming packets on the connection.
	handleUnknownPacket(p *proto.PacketContext) // Called to handle incoming unknown packet.
	disconnected()                              // Called when connection is closing, to teardown the session.

	activated()   // Called when the connection is now managed by this sessionHandler.
	deactivated() // Called when the connection is no longer managed by this sessionHandler.
}

// minecraftConn is a Minecraft connection from the
// client -> proxy or proxy -> server (backend).
type minecraftConn struct {
	proxy *Proxy   // convenient backreference
	c     net.Conn // Underlying connection

	// readLoop owns these fields
	readBuf *bufio.Reader
	decoder *codec.Decoder

	writeBuf *bufio.Writer
	encoder  *codec.Encoder

	//closed          chan struct{} // indicates connection is closed
	cancelFunc      context.CancelFunc
	closeOnce       sync.Once // Makes sure the connection is closed once, while blocking proceeding calls.
	closed          atomic.Bool
	knownDisconnect atomic.Bool // Silences disconnect (any error is known)

	protocol proto.Protocol // Client's protocol version.

	mu             sync.RWMutex    // Protects following fields
	state          *state.Registry // Client state.
	connType       connectionType  // Connection type
	sessionHandler sessionHandler  // The current session handler.
}

// newMinecraftConn returns a new Minecraft client connection.
func newMinecraftConn(base net.Conn, proxy *Proxy, playerConn bool, connDetails func() []zap.Field) (conn *minecraftConn) {
	in := proto.ServerBound  // reads from client are server bound (proxy <- client)
	out := proto.ClientBound // writes to client are client bound (proxy -> client)
	if !playerConn {         // if a backend server connection
		in = proto.ClientBound  // reads from backend are client bound (proxy <- backend)
		out = proto.ServerBound // writes to backend are server bound (proxy -> backend)
	}

	defer func() {
		conn.encoder = codec.NewEncoder(conn.writeBuf, out)
		conn.decoder = codec.NewDecoder(conn.readBuf, in, func() []zap.Field {
			return append(connDetails(), // TODO maybe fork a child zap logger for each connection
				zap.Stringer("remoteAddr", conn.RemoteAddr()),
			)
		})
	}()
	return &minecraftConn{
		proxy: proxy,
		c:     base,
		//closed:   make(chan struct{}),
		writeBuf: bufio.NewWriter(base),
		readBuf:  bufio.NewReader(base),
		state:    state.Handshake,
		protocol: proto.Minecraft_1_7_2.Protocol,
		connType: undeterminedConnectionType,
	}
}

// reads from underlying connection.
func (c *minecraftConn) nextPacket() (p *proto.PacketContext, err error) {
	p, err = c.decoder.ReadPacket()
	return
}

func loop(ctx context.Context, c *minecraftConn) bool {
	defer func() { // Catch any panics
		if r := recover(); r != nil {
			zap.S().Errorf("Recovered from panic in read packets loop: %v", r)
		}
	}()

	// Set read timeout to wait for client to send a packet
	deadline := time.Now().Add(time.Duration(c.config().ReadTimeout) * time.Millisecond)
	_ = c.c.SetReadDeadline(deadline)

	// Read next packet.
	packetCtx, err := c.nextPacket()
	if err != nil && !errors.Is(err, codec.ErrDecoderLeftBytes) { // Ignore this error.
		zap.L().Debug("Error reading packet", zap.Error(err))
		if handleReadErr(err) {
			// Sleep briefly and try again
			time.Sleep(time.Millisecond * 5)
			return true
		}
		return false
	}
	if !packetCtx.KnownPacket {
		c.SessionHandler().handleUnknownPacket(packetCtx)
		return true
	}

	// Handle packet by connections session handler.
	c.SessionHandler().handlePacket(ctx, packetCtx.Packet)
	return true
}

// readLoop is the main goroutine of this connection and
// reads packets to pass them further to the current sessionHandler.
// close will be called on method return.
func (c *minecraftConn) readLoop(ctx context.Context) {
	ctx, cancelFunc := context.WithCancel(ctx)
	c.cancelFunc = cancelFunc
	// Make sure to close connection on return, if not already
	defer func() { _ = c.closeKnown(false) }()
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if !loop(ctx, c) {
				break
			}
		}
	}
}

// handles error when read the next packet
func handleReadErr(err error) (recoverable bool) {
	var silentErr *errs.SilentError
	if errors.As(err, &silentErr) {
		zap.L().Debug("silentErr: error reading next packet, unrecoverable and closing connection", zap.Error(err))
		return false
	}
	// Immediately retry for EAGAIN
	if errors.Is(err, syscall.EAGAIN) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if netErr.Temporary() {
			// Immediately retry for temporary network errors
			return true
		} else if netErr.Timeout() {
			// Read timeout, disconnect
			zap.S().Errorf("read timeout: %v", err)
			return false
		} else if errs.IsConnClosedErr(netErr.Err) {
			// Connection is already closed
			return false
		}
	}
	// Immediately break for known unrecoverable errors
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrNoProgress) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.ErrShortBuffer) || errors.Is(err, syscall.EBADF) ||
		strings.Contains(err.Error(), "use of closed file") {
		return false
	}
	zap.L().Error("error reading next packet, unrecoverable and closing connection", zap.Error(err))
	return false
}

// Flush writes the buffered data to connection.
func (c *minecraftConn) flush() (err error) {
	defer func() { c.closeOnErr(err) }()
	deadline := time.Now().Add(time.Millisecond * time.Duration(c.config().ConnectionTimeout))
	if err = c.c.SetWriteDeadline(deadline); err != nil {
		// Handle err in case the connection is
		// already closed and can't write to.
		return err
	}
	// Must flush in sync with encoder or we may get an
	// io.ErrShortWrite when flushing while encoder is writing.
	return c.encoder.Sync(c.writeBuf.Flush)
}

func (c *minecraftConn) closeOnErr(err error) {
	if err == nil {
		return
	}
	_ = c.close()
	if err == ErrClosedConn {
		return // Don't log this error
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && errs.IsConnClosedErr(opErr.Err) {
		return // Don't log this error
	}
	zap.L().Debug("Error writing packet, closing connection", zap.Error(err))
}

// WritePacket writes a packet to the connection's
// write buffer and flushes the complete buffer afterwards.
//
// The connection will be closed on any error encountered!
func (c *minecraftConn) WritePacket(p proto.Packet) (err error) {
	if c.Closed() {
		return ErrClosedConn
	}
	defer func() { c.closeOnErr(err) }()
	if err = c.BufferPacket(p); err != nil {
		return err
	}
	return c.flush()
}

// Write encodes and writes payload to the connection's
// write buffer and flushes the complete buffer afterwards.
func (c *minecraftConn) Write(payload []byte) (err error) {
	if c.Closed() {
		return ErrClosedConn
	}
	defer func() { c.closeOnErr(err) }()
	if _, err = c.encoder.Write(payload); err != nil {
		return err
	}
	return c.flush()
}

// BufferPacket writes a packet into the connection's write buffer.
func (c *minecraftConn) BufferPacket(packet proto.Packet) (err error) {
	if c.Closed() {
		return ErrClosedConn
	}
	defer func() { c.closeOnErr(err) }()
	_, err = c.encoder.WritePacket(packet)
	return err
}

// BufferPayload writes payload (containing packet id + data) to the connection's write buffer.
func (c *minecraftConn) BufferPayload(payload []byte) (err error) {
	if c.Closed() {
		return ErrClosedConn
	}
	defer func() { c.closeOnErr(err) }()
	_, err = c.encoder.Write(payload)
	return err
}

// returns the proxy's config
func (c *minecraftConn) config() *config.Config {
	return c.proxy.config
}

// close closes the connection, if not already,
// and calls disconnected() on the current sessionHandler.
// It is okay to call this method multiple times as it will only
// run once but blocks if currently closing.
func (c *minecraftConn) close() error {
	return c.closeKnown(true)
}

// Indicates a connection is already closed.
var ErrClosedConn = errors.New("connection is closed")

func (c *minecraftConn) closeKnown(markKnown bool) (err error) {
	alreadyClosed := true
	c.closeOnce.Do(func() {
		alreadyClosed = false
		if markKnown {
			c.knownDisconnect.Store(true)
		}

		c.cancelFunc()
		c.closed.Store(true)
		err = c.c.Close()

		if sh := c.SessionHandler(); sh != nil {
			sh.disconnected()

			if p, ok := sh.(interface{ player_() *connectedPlayer }); ok && !c.knownDisconnect.Load() {
				zap.S().Infof("%s has disconnected", p.player_())
			}
		}

	})
	if alreadyClosed {
		err = ErrClosedConn
	}
	return err
}

// Closes the connection after writing the packet.
func (c *minecraftConn) closeWith(packet proto.Packet) (err error) {
	if c.Closed() {
		return ErrClosedConn
	}
	defer func() {
		err = c.close()
	}()

	//c.mu.Lock()
	//p := c.protocol
	//s := c.state
	//c.mu.Unlock()

	//is18 := p.GreaterEqual(proto.Minecraft_1_8)
	//isLegacyPing := s == state.Handshake || s == state.Status
	//if is18 || isLegacyPing {
	c.knownDisconnect.Store(true)
	_ = c.WritePacket(packet)
	//} else {
	// ??? 1.7.x versions have a race condition with switching protocol versions,
	// so just explicitly close the connection after a short while.
	// c.setAutoReading(false)
	//go func() {
	//	time.Sleep(time.Millisecond * 250)
	//	c.knownDisconnect.Store(true)
	//	_ = c.WritePacket(packet)
	//}()
	//}
	return
}

// Closed returns true if the connection is closed.
func (c *minecraftConn) Closed() bool {
	return c.closed.Load()
}

func (c *minecraftConn) RemoteAddr() net.Addr {
	return c.c.RemoteAddr()
}

func (c *minecraftConn) Protocol() proto.Protocol {
	return c.protocol
}

// setProtocol sets the connection's protocol version.
func (c *minecraftConn) setProtocol(protocol proto.Protocol) {
	c.protocol = protocol
	c.decoder.SetProtocol(protocol)
	c.encoder.SetProtocol(protocol)
	// TODO remove minecraft de/encoder when legacy handshake handling
}

func (c *minecraftConn) State() *state.Registry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

func (c *minecraftConn) setState(state *state.Registry) {
	c.mu.Lock()
	c.state = state
	c.decoder.SetState(state)
	c.encoder.SetState(state)
	c.mu.Unlock()
}

func (c *minecraftConn) Type() connectionType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connType
}

func (c *minecraftConn) setType(connType connectionType) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connType = connType
}

func (c *minecraftConn) SessionHandler() sessionHandler {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionHandler
}

// setSessionHandler sets the session handle for this connection
// and calls deactivated() on the old and activated() on the new.
func (c *minecraftConn) setSessionHandler(handler sessionHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setSessionHandler0(handler)
}

// same as setSessionHandler but without mutex locking
func (c *minecraftConn) setSessionHandler0(handler sessionHandler) {
	if c.sessionHandler != nil {
		c.sessionHandler.deactivated()
	}
	c.sessionHandler = handler
	handler.activated()
}

// Sets the compression threshold on the connection.
// You are responsible for sending packet.SetCompression beforehand.
func (c *minecraftConn) SetCompressionThreshold(threshold int) error {
	zap.S().Debugf("Set compression threshold %d", threshold)
	c.decoder.SetCompressionThreshold(threshold)
	return c.encoder.SetCompression(threshold, c.config().Compression.Level)
}

// SendKeepAlive sends a keep-alive packet to the connection if in Play state.
func (c *minecraftConn) SendKeepAlive() error {
	if c.State() == state.Play {
		return c.WritePacket(&packet.KeepAlive{RandomId: int64(randomUint64())})
	}
	return nil
}

// takes the secret key negotiated between the client and the
// server to enable encryption on the connection.
func (c *minecraftConn) enableEncryption(secret []byte) error {
	decryptReader, err := codec.NewDecryptReader(c.readBuf, secret)
	if err != nil {
		return err
	}
	encryptWriter, err := codec.NewEncryptWriter(c.writeBuf, secret)
	if err != nil {
		return err
	}
	c.decoder.SetReader(decryptReader)
	c.encoder.SetWriter(encryptWriter)
	return nil
}

// Inbound is an incoming connection to the proxy.
type Inbound interface {
	Protocol() proto.Protocol // The current protocol version the connection uses.
	VirtualHost() net.Addr    // The hostname, the client sent us, to join the server, if applicable.
	RemoteAddr() net.Addr     // The player's IP address.
	Active() bool             // Whether or not connection remains active.
	// Closed returns a receive only channel that can be used know when the connection was closed.
	// (e.g. for canceling work in an event subscriber)
	Closed() bool
}
