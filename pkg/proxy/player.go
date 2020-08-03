package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/common/minecraft/component/codec"
	"go.minekube.com/common/minecraft/component/codec/legacy"
	"go.minekube.com/gate/pkg/proto"
	"go.minekube.com/gate/pkg/proto/packet"
	"go.minekube.com/gate/pkg/proto/packet/plugin"
	"go.minekube.com/gate/pkg/proxy/forge"
	"go.minekube.com/gate/pkg/proxy/message"
	"go.minekube.com/gate/pkg/proxy/permission"
	"go.minekube.com/gate/pkg/proxy/player"
	"go.minekube.com/gate/pkg/util"
	"go.minekube.com/gate/pkg/util/gameprofile"
	"go.minekube.com/gate/pkg/util/modinfo"
	"go.minekube.com/gate/pkg/util/sets"
	"go.minekube.com/gate/pkg/util/uuid"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"net"
	"strings"
	"sync"
	"time"
)

// Player is a connected Minecraft player.
type Player interface {
	Inbound
	CommandSource
	message.ChannelMessageSource
	message.ChannelMessageSink

	Username() string // The username of the player.
	Id() uuid.UUID    // The Minecraft UUID of the player.
	// May be nil, if no backend server connection!
	CurrentServer() ServerConnection // Returns the current server connection of the player.
	Ping() time.Duration             // The player's ping or -1 if currently unknown.
	OnlineMode() bool                // Whether the player was authenticated with Mojang's session servers.
	// Creates a connection request to begin switching the backend server.
	CreateConnectionRequest(target RegisteredServer) ConnectionRequest
	GameProfile() *gameprofile.GameProfile // Returns the player's game profile.
	// TODO updateable game properties (locked func)
	// TabList() TODO
	// Disconnects the player with a reason.
	// Once called, further interface calls to this player become undefined.
	Disconnect(reason component.Component)
	// Sends chats input onto the player's current server as if
	// they typed it into the client chat box.
	SpoofChatInput(input string) error
	// Sends the specified resource pack from url to the user. If at all possible, send the
	// resource pack with a sha1 hash using SendResourcePackWithHash. To monitor the status of the
	// sent resource pack, subscribe to PlayerResourcePackStatusEvent.
	SendResourcePack(url string) error
	// Sends the specified resource pack from url to the user, using the specified 20-byte
	// SHA-1 hash of the resource pack file. To monitor the status of the sent resource pack,
	// subscribe to PlayerResourcePackStatusEvent.
	SendResourcePackWithHash(url string, sha1Hash []byte) error
	// TODO
}

// CommandSource is the source that ran a command.
type CommandSource interface {
	permission.Subject
	// Sends a message component to the invoker.
	SendMessage(msg component.Component) error
}

type connectedPlayer struct {
	*minecraftConn
	virtualHost net.Addr
	onlineMode  bool
	profile     *gameprofile.GameProfile
	ping        atomic.Duration
	permFunc    permission.Func

	// This field is true if this connection is being disconnected
	// due to another connection logging in with the same GameProfile.
	disconnectDueToDuplicateConnection atomic.Bool

	pluginChannelsMu sync.RWMutex // Protects following field
	pluginChannels   sets.String  // Known plugin channels

	mu               sync.RWMutex // Protects following fields
	connectedServer_ *serverConnection
	connInFlight     *serverConnection
	settings         player.Settings
	modInfo          *modinfo.ModInfo
	connPhase        clientConnectionPhase

	serversToTry []string // names of servers to try if we got disconnected from previous
	tryIndex     int
}

var _ Player = (*connectedPlayer)(nil)

func newConnectedPlayer(
	conn *minecraftConn,
	profile *gameprofile.GameProfile,
	virtualHost net.Addr,
	onlineMode bool,
) *connectedPlayer {
	ping := atomic.Duration{}
	ping.Store(-1)
	return &connectedPlayer{
		minecraftConn:  conn,
		profile:        profile,
		virtualHost:    virtualHost,
		onlineMode:     onlineMode,
		pluginChannels: sets.NewString(), // Should we limit the size to 1024 channels?
		connPhase:      conn.Type().initialClientPhase(),
		ping:           ping,
		permFunc:       func(string) permission.TriState { return permission.Undefined },
	}
}

func (p *connectedPlayer) connectionInFlight() *serverConnection {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connInFlight
}

func (p *connectedPlayer) phase() clientConnectionPhase {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connPhase
}

func (p *connectedPlayer) HasPermission(permission string) bool {
	return p.PermissionValue(permission).Bool()
}

func (p *connectedPlayer) PermissionValue(permission string) permission.TriState {
	return p.permFunc(permission)
}

func (p *connectedPlayer) Ping() time.Duration {
	return p.ping.Load()
}

func (p *connectedPlayer) OnlineMode() bool {
	return p.onlineMode
}

func (p *connectedPlayer) GameProfile() *gameprofile.GameProfile {
	return p.profile
}

var (
	ErrNoBackendConnection = errors.New("player has no backend server connection yet")
	ErrTooLongChatMessage  = errors.New("server bound chat message can not exceed 256 characters")
)

func (p *connectedPlayer) SpoofChatInput(input string) error {
	if len(input) > packet.MaxServerBoundMessageLength {
		return ErrTooLongChatMessage
	}

	serverMc, ok := p.ensureBackendConnection()
	if !ok {
		return ErrNoBackendConnection
	}
	return serverMc.WritePacket(&packet.Chat{
		Message: input,
		Type:    packet.ChatMessage,
	})
}

func (p *connectedPlayer) ensureBackendConnection() (*minecraftConn, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.connectedServer_ == nil {
		// Player has no backend connection.
		return nil, false
	}
	serverMc := p.connectedServer_.conn()
	if serverMc == nil {
		// Player's backend connection is not yet connected to a server.
		return nil, false
	}
	return serverMc, true
}

func (p *connectedPlayer) SendResourcePack(url string) error {
	return p.WritePacket(&packet.ResourcePackRequest{
		Url:  url,
		Hash: "",
	})
}

func (p *connectedPlayer) SendResourcePackWithHash(url string, sha1Hash []byte) error {
	if len(sha1Hash) != 20 {
		return errors.New("hash length must be 20")
	}
	return p.WritePacket(&packet.ResourcePackRequest{
		Url:  url,
		Hash: hex.EncodeToString(sha1Hash),
	})
}

func (p *connectedPlayer) VirtualHost() net.Addr {
	return p.virtualHost
}

func (p *connectedPlayer) Active() bool {
	return !p.minecraftConn.Closed()
}

func (p *connectedPlayer) SendMessage(msg component.Component) error {
	return p.SendMessagePosition(msg, packet.ChatMessage)
}

func (p *connectedPlayer) SendMessagePosition(msg component.Component, position packet.MessagePosition) (err error) {
	var messageJson string
	b := new(strings.Builder)
	if position == packet.ActionBarMessage {
		if p.Protocol().GreaterEqual(proto.Minecraft_1_11) {
			if err = util.JsonCodec(p.Protocol()).Marshal(b, msg); err != nil {
				return err
			}
			s := b.String()
			// We can use the title packet instead
			return p.WritePacket(&packet.Title{
				Action:    packet.SetActionBar,
				Component: &s,
			})
		}
		// Due to issues with action bar packets, we'll need to convert the text message into a
		// legacy message and then put the legacy text into a component... (╯°□°)╯︵ ┻━┻!
		if err = (&legacy.Legacy{}).Marshal(b, msg); err != nil {
			return err
		}
		j, err := json.Marshal(map[string]string{
			"text": b.String(),
		})
		if err != nil {
			return err
		}
		messageJson = string(j)
	} else {
		if err = util.JsonCodec(p.Protocol()).Marshal(b, msg); err != nil {
			return err
		}
		messageJson = b.String()
	}

	return p.WritePacket(&packet.Chat{
		Message: messageJson,
		Type:    packet.ChatMessage,
		Sender:  uuid.Nil,
	})
}

func (p *connectedPlayer) SendPluginMessage(identifier message.ChannelIdentifier, data []byte) error {
	return p.WritePacket(&plugin.Message{
		Channel: identifier.Id(),
		Data:    data,
	})
}

// TODO add header/footer, action bar, title & boss bar methods

// Finds another server to attempt to log into, if we were unexpectedly disconnected from the server.
// current is the current server of the player is on, so we skip this server and not connect to it.
// current can be nil if there is no current server.
// MAY RETURN NIL if no next server available!
func (p *connectedPlayer) nextServerToTry(current RegisteredServer) RegisteredServer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.serversToTry) == 0 {
		p.serversToTry = p.proxy.Config().ForcedHosts[p.virtualHost.String()]
	}

	if len(p.serversToTry) == 0 {
		p.serversToTry = p.proxy.Config().AttemptConnectionOrder()
	}

	sameName := func(rs RegisteredServer, name string) bool {
		return rs.ServerInfo().Name() == name
	}

	for i := p.tryIndex; i < len(p.serversToTry); i++ {
		toTry := p.serversToTry[i]
		if (p.connectedServer_ != nil && sameName(p.connectedServer_.Server(), toTry)) ||
			(p.connInFlight != nil && sameName(p.connInFlight.Server(), toTry)) ||
			(current != nil && sameName(current, toTry)) {
			continue
		}

		p.tryIndex = i
		if s := p.proxy.Server(toTry); s != nil {
			return s
		}
	}
	return nil
}

// player's connection is closed at this point,
// now need to disconnect backend server connection, if any.
func (p *connectedPlayer) teardown() {
	p.mu.RLock()
	connInFlight := p.connInFlight
	connectedServer := p.connectedServer_
	p.mu.RUnlock()
	if connInFlight != nil {
		connInFlight.disconnect()
	}
	if connectedServer != nil {
		connectedServer.disconnect()
	}

	var status LoginStatus
	if p.proxy.connect.unregisterConnection(p) {
		if p.disconnectDueToDuplicateConnection.Load() {
			status = ConflictingLoginStatus
		} else {
			status = SuccessfulLoginStatus
		}
	} else {
		if p.knownDisconnect.Load() {
			status = CanceledByProxyLoginStatus
		} else {
			status = CanceledByUserLoginStatus
		}
	}
	p.proxy.event.Fire(&DisconnectEvent{
		player:      p,
		loginStatus: status,
	})
}

// may be nil!
func (p *connectedPlayer) CurrentServer() ServerConnection {
	sc := p.connectedServer()
	if sc == nil {
		// Fixes interface nil != nil // TODO check if still necessary
		return nil
	}
	return sc
}

func (p *connectedPlayer) connectedServer() *serverConnection {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connectedServer_
}

func (p *connectedPlayer) Username() string {
	return p.profile.Name
}

func (p *connectedPlayer) Id() uuid.UUID {
	return p.profile.Id
}

func (p *connectedPlayer) Disconnect(reason component.Component) {
	if !p.Active() {
		return
	}

	// TODO convert reason to legacy formatting and use logger that supports displays legacy minecraft colors
	var r interface{} = reason
	b := new(strings.Builder)
	if (&codec.Plain{}).Marshal(b, reason) == nil {
		r = b.String()
	}

	if p.closeWith(packet.DisconnectWithProtocol(reason, p.Protocol())) == nil {
		zap.S().Infof("%s has disconnected: %s", p, r)
	}
}

func (p *connectedPlayer) String() string {
	return p.profile.Name
}

func (p *connectedPlayer) sendLegacyForgeHandshakeResetPacket() {
	p.phase().resetConnectionPhase(p)
}

func (p *connectedPlayer) setPhase(phase *legacyForgeHandshakeClientPhase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connPhase = phase
}

// may return nil
func (p *connectedPlayer) ModInfo() *modinfo.ModInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.modInfo
}

func (p *connectedPlayer) SetModInfo(modInfo *modinfo.ModInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modInfo = modInfo
}

// NOTE: the returned set is not goroutine-safe and must not be modified,
// it is only for reading!!!
func (p *connectedPlayer) knownChannels() sets.String {
	p.pluginChannelsMu.RLock()
	defer p.pluginChannelsMu.RUnlock()
	return p.pluginChannels
}

// runs fn while pluginChannels is locked. Used for modifying channel set.
func (p *connectedPlayer) lockedKnownChannels(fn func(knownChannels sets.String)) {
	p.pluginChannelsMu.RUnlock()
	defer p.pluginChannelsMu.RLock()
	fn(p.pluginChannels)
}

// Determines whether or not we can forward a plugin message onto the client.
// message - plugin message to forward to the client
func (p *connectedPlayer) canForwardPluginMessage(protocol proto.Protocol, message *plugin.Message) bool {
	var minecraftOrFmlMessage bool

	// By default, all internal Minecraft and Forge channels are forwarded from the server.
	if int(protocol) <= int(proto.Minecraft_1_12_2.Protocol) {
		channel := message.Channel
		minecraftOrFmlMessage = strings.HasPrefix(channel, "MC|") ||
			strings.HasPrefix(channel, forge.LegacyHandshakeChannel) ||
			plugin.LegacyRegister(message) || plugin.LegacyUnregister(message)
	} else {
		minecraftOrFmlMessage = strings.HasPrefix(message.Channel, "minecraft:")
	}

	// Otherwise, we need to see if the player already knows this channel or it's known by the proxy.
	return minecraftOrFmlMessage || p.knownChannels().Has(message.Channel)
}

func (p *connectedPlayer) setConnectedServer(conn *serverConnection) {
	p.mu.Lock()
	p.connectedServer_ = conn
	p.mu.Unlock()
}

func (p *connectedPlayer) setSettings(settings *packet.ClientSettings) {
	wrapped := player.NewSettings(settings)
	p.mu.Lock()
	p.settings = wrapped
	p.mu.Unlock()

	p.proxy.Event().FireParallel(&PlayerSettingsChangedEvent{
		player:   p,
		settings: wrapped,
	})
}

// Settings returns the players client settings.
// If not known already, returns player.DefaultSettings.
func (p *connectedPlayer) Settings() player.Settings {
	p.mu.RLock()
	if p.settings != nil {
		return p.settings
	}
	p.mu.RUnlock()
	return player.DefaultSettings
}

func RandomUint64() uint64 {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf) // Always succeeds, no need to check error
	return binary.LittleEndian.Uint64(buf)
}
