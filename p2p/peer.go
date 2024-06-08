package p2p

import (
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/cosmos/gogoproto/proto"

	"github.com/cometbft/cometbft/internal/cmap"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cometbft/cometbft/libs/service"
	cmtconn "github.com/cometbft/cometbft/p2p/conn"
	"github.com/cometbft/cometbft/types"
)

//go:generate ../scripts/mockery_generate.sh Peer

const metricsTickerDuration = 10 * time.Second

// Peer is an interface representing a peer connected on a reactor.
type Peer interface {
	service.Service
	FlushStop()

	ID() ID               // peer's cryptographic ID
	RemoteIP() net.IP     // remote IP of the connection
	RemoteAddr() net.Addr // remote address of the connection

	IsOutbound() bool   // did we dial the peer
	IsPersistent() bool // do we redial this peer when we disconnect

	CloseConn() error // close original connection

	NodeInfo() NodeInfo // peer's info
	Status() cmtconn.ConnectionStatus
	SocketAddr() *NetAddress // actual address of the socket

	Send(e Envelope) bool
	TrySend(e Envelope) bool

	Set(key string, value any)
	Get(key string) any

	SetRemovalFailed()
	GetRemovalFailed() bool
}

// ----------------------------------------------------------

// peerConn contains the raw connection and its config.
type peerConn struct {
	outbound   bool
	persistent bool
	conn       net.Conn // Source connection

	socketAddr *NetAddress

	// cached RemoteIP()
	ip net.IP
}

func newPeerConn(
	outbound, persistent bool,
	conn net.Conn,
	socketAddr *NetAddress,
) peerConn {
	return peerConn{
		outbound:   outbound,
		persistent: persistent,
		conn:       conn,
		socketAddr: socketAddr,
	}
}

// ID only exists for SecretConnection.
// NOTE: Will panic if conn is not *SecretConnection.
func (pc peerConn) ID() ID {
	return PubKeyToID(pc.conn.(*cmtconn.SecretConnection).RemotePubKey())
}

// Return the IP from the connection RemoteAddr.
func (pc peerConn) RemoteIP() net.IP {
	if pc.ip != nil {
		return pc.ip
	}

	host, _, err := net.SplitHostPort(pc.conn.RemoteAddr().String())
	if err != nil {
		panic(err)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		panic(err)
	}

	pc.ip = ips[0]

	return pc.ip
}

// peer implements Peer.
//
// Before using a peer, you will need to perform a handshake on connection.
type peer struct {
	service.BaseService

	// raw peerConn and the multiplex connection
	peerConn
	mconn *cmtconn.MConnection

	// peer's node info and the channel it knows about
	// channels = nodeInfo.Channels
	// cached to avoid copying nodeInfo in hasChannel
	nodeInfo NodeInfo
	channels []byte

	// User data
	Data *cmap.CMap

	metrics *Metrics
	mlc     *metricsLabelCache

	setupRecvListeners    func()
	recvListenersQuitChan chan struct{}

	// When removal of a peer fails, we set this flag
	removalAttemptFailed bool
}

type PeerOption func(*peer)

func newPeer(
	pc peerConn,
	mConfig cmtconn.MConnConfig,
	nodeInfo NodeInfo,
	reactorsByCh map[byte]Reactor,
	msgTypeByChID map[byte]proto.Message,
	chDescs []*cmtconn.ChannelDescriptor,
	onPeerError func(Peer, any),
	mlc *metricsLabelCache,
	options ...PeerOption,
) *peer {
	p := &peer{
		peerConn: pc,
		nodeInfo: nodeInfo,
		channels: nodeInfo.(DefaultNodeInfo).Channels,
		Data:     cmap.NewCMap(),
		metrics:  NopMetrics(),
		mlc:      mlc,
	}

	// We process each channel's received packets concurrently,
	// in-order per channel. channelIDToProcessChannel is a map of channelID
	// to the go-channel for processing messages.
	channelIDToProcessChannel := make(map[byte]chan []byte)
	// However we want to actually start these threads when the peer is started,
	// so we queue the function we will send.
	p.setupRecvListeners = func() { p.setupChannelProcessors(reactorsByCh, msgTypeByChID, channelIDToProcessChannel) }
	p.mconn = createMConnection(
		pc.conn,
		p,
		channelIDToProcessChannel,
		chDescs,
		onPeerError,
		mConfig,
	)
	p.BaseService = *service.NewBaseService(nil, "Peer", p)
	for _, option := range options {
		option(p)
	}

	return p
}

// String representation.
func (p *peer) String() string {
	if p.outbound {
		return fmt.Sprintf("Peer{%v %v out}", p.mconn, p.ID())
	}

	return fmt.Sprintf("Peer{%v %v in}", p.mconn, p.ID())
}

// ---------------------------------------------------
// Implements service.Service

// SetLogger implements BaseService.
func (p *peer) SetLogger(l log.Logger) {
	p.Logger = l
	p.mconn.SetLogger(l)
}

// OnStart implements BaseService.
func (p *peer) OnStart() error {
	if err := p.BaseService.OnStart(); err != nil {
		return err
	}

	p.recvListenersQuitChan = make(chan struct{}, len(p.channels))
	p.setupRecvListeners()

	if err := p.mconn.Start(); err != nil {
		return err
	}

	go p.metricsReporter()
	return nil
}

// FlushStop mimics OnStop but additionally ensures that all successful
// .Send() calls will get flushed before closing the connection.
// Correct API usage requires FlushStop to terminate, and then OnStop getting called.
//
// NOTE: it is not safe to call this method more than once.
func (p *peer) FlushStop() {
	p.mconn.FlushStop() // stop everything and close the conn
}

// OnStop implements BaseService.
func (p *peer) OnStop() {
	if err := p.mconn.Stop(); err != nil { // stop everything and close the conn
		p.Logger.Debug("Error while stopping peer", "err", err)
	}
	for i := 0; i < len(p.channels); i++ {
		p.recvListenersQuitChan <- struct{}{}
	}
	close(p.recvListenersQuitChan)
}

// ---------------------------------------------------
// Implements Peer

// ID returns the peer's ID - the hex encoded hash of its pubkey.
func (p *peer) ID() ID {
	return p.nodeInfo.ID()
}

// IsOutbound returns true if the connection is outbound, false otherwise.
func (p *peer) IsOutbound() bool {
	return p.peerConn.outbound
}

// IsPersistent returns true if the peer is persistent, false otherwise.
func (p *peer) IsPersistent() bool {
	return p.peerConn.persistent
}

// NodeInfo returns a copy of the peer's NodeInfo.
func (p *peer) NodeInfo() NodeInfo {
	return p.nodeInfo
}

// SocketAddr returns the address of the socket.
// For outbound peers, it's the address dialed (after DNS resolution).
// For inbound peers, it's the address returned by the underlying connection
// (not what's reported in the peer's NodeInfo).
func (p *peer) SocketAddr() *NetAddress {
	return p.peerConn.socketAddr
}

// Status returns the peer's ConnectionStatus.
func (p *peer) Status() cmtconn.ConnectionStatus {
	return p.mconn.Status()
}

// Send msg bytes to the channel identified by chID byte. Returns false if the
// send queue is full after timeout, specified by MConnection.
//
// thread safe.
func (p *peer) Send(e Envelope) bool {
	return p.send(e.ChannelID, e.Message, p.mconn.Send)
}

// TrySend msg bytes to the channel identified by chID byte. Immediately returns
// false if the send queue is full.
//
// thread safe.
func (p *peer) TrySend(e Envelope) bool {
	return p.send(e.ChannelID, e.Message, p.mconn.TrySend)
}

func (p *peer) send(chID byte, msg proto.Message, sendFunc func(byte, []byte) bool) bool {
	if !p.IsRunning() {
		return false
	} else if !p.hasChannel(chID) {
		return false
	}
	metricLabelValue := p.mlc.ValueToMetricLabel(msg)
	if w, ok := msg.(types.Wrapper); ok {
		msg = w.Wrap()
	}
	msgBytes, err := proto.Marshal(msg)
	if err != nil {
		p.Logger.Error("marshaling message to send", "error", err)
		return false
	}
	res := sendFunc(chID, msgBytes)
	if res {
		p.metrics.PeerSendBytesTotal.
			With("peer_id", string(p.ID()), "chID", p.mlc.ChIDToMetricLabel(chID)).
			Add(float64(len(msgBytes)))
		p.metrics.MessageSendBytesTotal.
			With("message_type", metricLabelValue).
			Add(float64(len(msgBytes)))
	}
	return res
}

// Get the data for a given key.
//
// thread safe.
func (p *peer) Get(key string) any {
	return p.Data.Get(key)
}

// Set sets the data for the given key.
//
// thread safe.
func (p *peer) Set(key string, data any) {
	p.Data.Set(key, data)
}

// hasChannel returns true if the peer reported
// knowing about the given chID.
func (p *peer) hasChannel(chID byte) bool {
	for _, ch := range p.channels {
		if ch == chID {
			return true
		}
	}
	// NOTE: probably will want to remove this
	// but could be helpful while the feature is new
	p.Logger.Debug(
		"Unknown channel for peer",
		"channel",
		chID,
		"channels",
		p.channels,
	)
	return false
}

// CloseConn closes original connection. Used for cleaning up in cases where the peer had not been started at all.
func (p *peer) CloseConn() error {
	return p.peerConn.conn.Close()
}

func (p *peer) SetRemovalFailed() {
	p.removalAttemptFailed = true
}

func (p *peer) GetRemovalFailed() bool {
	return p.removalAttemptFailed
}

// ---------------------------------------------------
// methods only used for testing
// TODO: can we remove these?

// CloseConn closes the underlying connection.
func (pc *peerConn) CloseConn() {
	pc.conn.Close()
}

// RemoteAddr returns peer's remote network address.
func (p *peer) RemoteAddr() net.Addr {
	return p.peerConn.conn.RemoteAddr()
}

// CanSend returns true if the send queue is not full, false otherwise.
func (p *peer) CanSend(chID byte) bool {
	if !p.IsRunning() {
		return false
	}
	return p.mconn.CanSend(chID)
}

// ---------------------------------------------------

func PeerMetrics(metrics *Metrics) PeerOption {
	return func(p *peer) {
		p.metrics = metrics
	}
}

func (p *peer) metricsReporter() {
	metricsTicker := time.NewTicker(metricsTickerDuration)
	defer metricsTicker.Stop()

	for {
		select {
		case <-metricsTicker.C:
			status := p.mconn.Status()
			var sendQueueSize float64
			for _, chStatus := range status.Channels {
				sendQueueSize += float64(chStatus.SendQueueSize)
			}

			p.metrics.PeerPendingSendBytes.With("peer_id", string(p.ID())).Set(sendQueueSize)
		case <-p.Quit():
			return
		}
	}
}

// ------------------------------------------------------------------
// helper funcs

// returns an mconnection, and a map of channelID to channel for processing messages.
func createMConnection(
	conn net.Conn,
	p *peer,
	channelIDToProcessChannel map[byte]chan []byte,
	chDescs []*cmtconn.ChannelDescriptor,
	onPeerError func(Peer, any),
	config cmtconn.MConnConfig,
) *cmtconn.MConnection {
	onError := func(r any) {
		onPeerError(p, r)
	}

	onReceive := func(chID byte, msgBytes []byte) {
		// TODO: Consider sync.Pool for this, upon examining benchmarks
		chanBytes := make([]byte, len(msgBytes))
		copy(chanBytes, msgBytes)

		channel, ok := channelIDToProcessChannel[chID]
		if !ok {
			panic(fmt.Sprintf("Unknown channel %X", chID))
		}
		channel <- chanBytes
	}

	return cmtconn.NewMConnectionWithConfig(
		conn,
		chDescs,
		onReceive,
		onError,
		config,
	)
}

// Setsup the go-channels for every logical channel,
// and a goroutine to process incoming messages on each logical channel.
// The channelIDToProcessChannel argument is updated for every channel here.
func (p *peer) setupChannelProcessors(
	reactorsByCh map[byte]Reactor,
	msgTypeByChID map[byte]proto.Message,
	channelIDToProcessChannel map[byte]chan []byte,
) {
	// setup channel processors
	for i := 0; i < 256; i++ {
		chID := byte(i)
		reactor, ok1 := reactorsByCh[chID]
		msgType, ok2 := msgTypeByChID[chID]
		if !(ok1 && ok2) || reactor == nil {
			continue
		}
		// allow 256 messages to be in proto-processing queue per channel.
		// Note that every reactor should itself buffer messages.
		// 256 is a magic constant that can be optimized later, but likely can
		// remain very small. (This is more justified once there is a buffer per-reactor)
		channel := make(chan []byte, 256)
		channelIDToProcessChannel[chID] = channel
		go channelProcessor(p, reactor, chID, msgType, channel)
	}
}

// Processes incoming packets for this channel. This should be ran in a go-routine.
// when it hits an error, it panics, which is safe as it must be caught by a conn.PanicRecover.
func channelProcessor(p *peer, reactor Reactor, chID byte, msgType proto.Message, channel chan []byte) {
	defer p.mconn.PanicRecover()
	for {
		select {
		case msgBytes := <-channel:
			// TODO: Re-architect to keep already-cloned copies ready to go
			mt := msgType
			msg := proto.Clone(mt)
			err := proto.Unmarshal(msgBytes, msg)
			if err != nil {
				panic(fmt.Sprintf("unmarshaling message: %v into type: %s", err, reflect.TypeOf(mt)))
			}
			if w, ok := msg.(types.Unwrapper); ok {
				msg, err = w.Unwrap()
				if err != nil {
					panic(fmt.Sprintf("unwrapping message: %v", err))
				}
			}
			p.metrics.PeerReceiveBytesTotal.
				With("peer_id", string(p.ID()), "chID", p.mlc.ChIDToMetricLabel(chID)).
				Add(float64(len(msgBytes)))
			p.metrics.MessageReceiveBytesTotal.
				With("message_type", p.mlc.ValueToMetricLabel(msg)).
				Add(float64(len(msgBytes)))
			reactor.Receive(Envelope{
				ChannelID: chID,
				Src:       p,
				Message:   msg,
			})
		case <-p.recvListenersQuitChan:
			return
		}
	}
}
