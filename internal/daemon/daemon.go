// Package daemon is the always-on core: peer discovery, encrypted
// connections, chat delivery and file transfer. The TUI and CLI are
// thin clients of this process over a local unix socket.
package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"chuanshu/internal/appcfg"
	"chuanshu/internal/identity"
	"chuanshu/internal/store"
	"chuanshu/internal/wire"
)

// BuildInfo is stamped by main so clients can tell which build
// the running daemon process actually is (a replaced binary does
// not change an already-running process).
var BuildInfo = "dev"

// Event is pushed to subscribed IPC clients.
type Event struct {
	Kind string `json:"kind"`
	Data any    `json:"data,omitempty"`
}

// peerConn is a live encrypted session to one peer.
type peerConn struct {
	peerID   string
	nickname string
	sess     *wire.Session
}

// Daemon is the core process state.
type Daemon struct {
	Cfg   appcfg.Config
	Paths appcfg.Paths
	ID    *identity.Identity
	Store *store.Store

	mu             sync.Mutex
	conns          map[string]*peerConn
	pendingAck     map[string]chan string
	pendingOffer   map[string]chan offerResult
	pendingDone    map[string]chan error
	incomingOffers map[string]*incomingOffer
	incoming       map[string]*incomingTransfer
	acceptedGroups map[string]bool
	subs           map[chan Event]struct{}
	listener       net.Listener
	startedAt      time.Time
}

type offerResult struct {
	accept bool
	offset int64
	reason string
}

// New builds a daemon (does not start listeners).
func New(cfg appcfg.Config, paths appcfg.Paths) (*Daemon, error) {
	id, err := identity.LoadOrCreate(paths.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(paths.DataDir)
	if err != nil {
		return nil, err
	}
	return &Daemon{
		Cfg: cfg, Paths: paths, ID: id, Store: st,
		conns:          map[string]*peerConn{},
		pendingAck:     map[string]chan string{},
		pendingOffer:   map[string]chan offerResult{},
		pendingDone:    map[string]chan error{},
		incomingOffers: map[string]*incomingOffer{},
		incoming:       map[string]*incomingTransfer{},
		acceptedGroups: map[string]bool{},
		subs:           map[chan Event]struct{}{},
		startedAt:      time.Now(),
	}, nil
}

// Me returns this machine's identity summary.
func (d *Daemon) Me() map[string]any {
	return map[string]any{
		"peer_id":     d.ID.MyPeerID(),
		"nickname":    d.Cfg.Nickname,
		"code":        d.ID.MyCode(),
		"fingerprint": identity.Fingerprint(d.ID.NoisePublicBytes()),
		"tcp_port":    d.Cfg.TCPPort,
		"build":       BuildInfo,
	}
}

// Start brings up TCP listener, discovery and IPC, then blocks.
func (d *Daemon) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", d.Cfg.TCPPort))
	if err != nil {
		return fmt.Errorf("listen tcp %d: %w", d.Cfg.TCPPort, err)
	}
	d.listener = ln
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		d.mu.Lock()
		for id, pc := range d.conns {
			_ = pc.sess.Close()
			delete(d.conns, id)
		}
		d.mu.Unlock()
	}()
	log.Printf("chuanshu daemon: peer %s (%s) tcp :%d udp :%d", d.ID.MyPeerID(), d.Cfg.Nickname, d.Cfg.TCPPort, d.Cfg.DiscoveryUDPPort)
	go d.acceptLoop(ctx)
	go d.discoverLoop(ctx)
	go d.presenceLoop(ctx)
	if err := d.serveIPC(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (d *Daemon) acceptLoop(ctx context.Context) {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			sess, err := wire.AcceptSession(conn, d.ID)
			if err != nil {
				log.Printf("inbound handshake failed: %v", err)
				_ = conn.Close()
				return
			}
			d.registerConn(sess, false)
		}()
	}
}

func (d *Daemon) presenceLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.emit(Event{Kind: "peers", Data: d.Peers()})
		}
	}
}

// registerConn installs a session, performs the hello exchange and
// starts the read loop. Existing live connections win.
func (d *Daemon) registerConn(sess *wire.Session, outbound bool) *peerConn {
	peerID := identity.PeerID(sess.RemotePub)
	d.mu.Lock()
	if old, ok := d.conns[peerID]; ok {
		d.mu.Unlock()
		// Keep the existing connection; drop the duplicate.
		go func() {
			// Give the kept connection's hello a chance; just close.
			_ = sess.Close()
			_ = old
		}()
		return old
	}
	pc := &peerConn{peerID: peerID, sess: sess}
	d.conns[peerID] = pc
	d.mu.Unlock()

	// Pin or verify identity (TOFU). peerID is derived from the key,
	// so a stored record with a different key under this id means
	// local state corruption or an attack: refuse.
	noiseB64 := base64.StdEncoding.EncodeToString(sess.RemotePub)
	if rec, ok := d.Store.Peer(peerID); ok && rec.NoisePub != "" && rec.NoisePub != noiseB64 {
		log.Printf("WARNING: key mismatch for peer %s, dropping", peerID)
		d.dropConn(peerID)
		return nil
	}
	_, _ = d.Store.UpsertPeer(&store.PeerRecord{
		PeerID: peerID, NoisePub: noiseB64, LastSeen: time.Now(),
	})
	hello := wire.Message{
		Type: wire.TypeHello, Nickname: d.Cfg.Nickname,
		SignPublic: base64.StdEncoding.EncodeToString(d.ID.SignPublicBytes()),
	}
	_ = sess.SendMessage(hello)
	go d.readLoop(pc)
	d.emit(Event{Kind: "peers", Data: d.Peers()})
	return pc
}

func (d *Daemon) dropConn(peerID string) {
	d.mu.Lock()
	pc, ok := d.conns[peerID]
	if ok {
		delete(d.conns, peerID)
	}
	d.mu.Unlock()
	if ok {
		_ = pc.sess.Close()
	}
	d.emit(Event{Kind: "peers", Data: d.Peers()})
}

func (d *Daemon) connFor(peerID string) (*peerConn, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pc, ok := d.conns[peerID]
	return pc, ok
}

// ensureConn returns a live connection, dialing if the peer is
// known to be reachable. Offline peers fail fast, per spec.
func (d *Daemon) ensureConn(peerID string) (*peerConn, error) {
	if pc, ok := d.connFor(peerID); ok {
		return pc, nil
	}
	rec, ok := d.Store.Peer(peerID)
	if !ok || rec.Addr == "" {
		return nil, fmt.Errorf("对方不在线")
	}
	if !rec.Manual && time.Since(rec.LastSeen) > 60*time.Second {
		return nil, fmt.Errorf("对方不在线")
	}
	conn, err := net.DialTimeout("tcp", rec.Addr, 4*time.Second)
	if err != nil {
		return nil, fmt.Errorf("对方不在线")
	}
	sess, err := wire.DialSession(conn, d.ID)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("与对方建立加密连接失败: %w", err)
	}
	pc := d.registerConn(sess, true)
	if pc == nil {
		return nil, fmt.Errorf("对方身份校验失败")
	}
	return pc, nil
}

// Peers returns the roster with computed online status.
func (d *Daemon) Peers() []*store.PeerRecord {
	peers := d.Store.Peers()
	d.mu.Lock()
	conns := d.conns
	d.mu.Unlock()
	for _, p := range peers {
		_, live := conns[p.PeerID]
		p.Online = live || (!p.LastSeen.IsZero() && time.Since(p.LastSeen) < 60*time.Second)
	}
	return peers
}

// ResolvePeer resolves a user query to a roster entry.
func (d *Daemon) ResolvePeer(query string) (*store.PeerRecord, error) {
	if query == store.BroadcastConv {
		return &store.PeerRecord{PeerID: store.BroadcastConv, Nickname: "全员广播"}, nil
	}
	p, ok := d.Store.Resolve(query)
	if !ok {
		return nil, fmt.Errorf("找不到同事 %q（先确认对方在线并出现在列表里）", query)
	}
	return p, nil
}

// emit fans an event out to subscribers (non-blocking).
func (d *Daemon) emit(e Event) {
	d.mu.Lock()
	subs := make([]chan Event, 0, len(d.subs))
	for ch := range d.subs {
		subs = append(subs, ch)
	}
	d.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default: // slow subscriber drops events, never blocks daemon
		}
	}
}

func (d *Daemon) subscribe() chan Event {
	ch := make(chan Event, 64)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *Daemon) unsubscribe(ch chan Event) {
	d.mu.Lock()
	delete(d.subs, ch)
	d.mu.Unlock()
}

func (d *Daemon) notify(title, body string) {
	go func() {
		cmd := exec.Command("notify-send", "-a", "chuanshu", title, body)
		_ = cmd.Run()
	}()
}

// touchPeer marks a peer seen just now.
func (d *Daemon) touchPeer(peerID, nickname, addr string) {
	rec := &store.PeerRecord{PeerID: peerID, LastSeen: time.Now()}
	if nickname != "" {
		rec.Nickname = nickname
	}
	if addr != "" {
		rec.Addr = addr
	}
	_, _ = d.Store.UpsertPeer(rec)
}
