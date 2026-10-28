package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"chuanshu/internal/identity"
	"chuanshu/internal/store"
	"chuanshu/internal/wire"

	"github.com/hashicorp/mdns"
	"golang.org/x/sys/unix"
)

// announce is the signed UDP presence packet.
type announce struct {
	PeerID    string `json:"peer_id"`
	Nickname  string `json:"nickname"`
	TCPPort   int    `json:"tcp_port"`
	NoisePub  string `json:"noise_pub"`
	SignPub   string `json:"sign_pub"`
	TS        int64  `json:"ts"`
	Signature string `json:"sig"`
}

func (a *announce) canonical() string {
	return fmt.Sprintf("%s|%s|%d|%s|%d", a.PeerID, a.Nickname, a.TCPPort, a.NoisePub, a.TS)
}

// discoverLoop runs UDP broadcast presence and mDNS together; either
// channel alone is enough to find peers.
func (d *Daemon) discoverLoop(ctx context.Context) {
	go d.udpAnnounceLoop(ctx)
	go d.udpListenLoop(ctx)
	if os.Getenv("CHUANSHU_DISABLE_MDNS") == "" {
		go d.mdnsLoop(ctx)
	}
}

func (d *Daemon) myAnnounce() *announce {
	a := &announce{
		PeerID:   d.ID.MyPeerID(),
		Nickname: d.Cfg.Nickname,
		TCPPort:  d.Cfg.TCPPort,
		NoisePub: base64.StdEncoding.EncodeToString(d.ID.NoisePublicBytes()),
		SignPub:  base64.StdEncoding.EncodeToString(d.ID.SignPublicBytes()),
		TS:       time.Now().Unix(),
	}
	a.Signature = base64.StdEncoding.EncodeToString(d.ID.Sign([]byte(a.canonical())))
	return a
}

func (d *Daemon) udpAnnounceLoop(ctx context.Context) {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", d.Cfg.DiscoveryUDPPort))
	if err != nil {
		return
	}
	// Sending to the broadcast address requires SO_BROADCAST.
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		})
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		log.Printf("udp announce socket: %v", err)
		return
	}
	defer pc.Close()
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	send := func() {
		raw, err := json.Marshal(d.myAnnounce())
		if err != nil {
			return
		}
		_, _ = pc.WriteTo(raw, addr)
		// Also hit each subnet's directed broadcast address: some
		// routers and drivers treat it more reliably than the
		// global broadcast address.
		for _, dest := range subnetBroadcasts(d.Cfg.DiscoveryUDPPort) {
			_, _ = pc.WriteTo(raw, dest)
		}
	}
	send()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

// subnetBroadcasts computes directed broadcast addresses for all
// up, non-loopback IPv4 interfaces.
func subnetBroadcasts(port int) []*net.UDPAddr {
	var out []*net.UDPAddr
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip := ipnet.IP.To4()
			bcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bcast[i] = ip[i] | ^ipnet.Mask[i]
			}
			out = append(out, &net.UDPAddr{IP: bcast, Port: port})
		}
	}
	return out
}

func (d *Daemon) udpListenLoop(ctx context.Context) {
	// Reuse the port so several daemons on one host (multi-user
	// machines, local testing) all hear broadcasts.
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf(":%d", d.Cfg.DiscoveryUDPPort))
	if err != nil {
		log.Printf("udp listen: %v", err)
		return
	}
	defer pc.Close()
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	buf := make([]byte, 4096)
	for {
		n, remote, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var a announce
		if err := json.Unmarshal(buf[:n], &a); err != nil {
			continue
		}
		d.handleAnnounce(&a, remote)
	}
}

func (d *Daemon) handleAnnounce(a *announce, remote net.Addr) {
	if a.PeerID == d.ID.MyPeerID() || a.PeerID == "" {
		return
	}
	noisePub, err := base64.StdEncoding.DecodeString(a.NoisePub)
	if err != nil || identity.PeerID(noisePub) != a.PeerID {
		return
	}
	signPub, err := base64.StdEncoding.DecodeString(a.SignPub)
	if err != nil {
		return
	}
	sig, err := base64.StdEncoding.DecodeString(a.Signature)
	if err != nil || !identity.Verify(signPub, []byte(a.canonical()), sig) {
		return
	}
	if time.Since(time.Unix(a.TS, 0)) > 2*time.Minute {
		return
	}
	host := remote.String()
	if ua, ok := remote.(*net.UDPAddr); ok {
		host = ua.IP.String()
	} else if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr := net.JoinHostPort(host, strconv.Itoa(a.TCPPort))
	_, _ = d.Store.UpsertPeer(&store.PeerRecord{
		PeerID: a.PeerID, Nickname: a.Nickname, NoisePub: a.NoisePub,
		SignPub: a.SignPub, Addr: addr, LastSeen: time.Now(),
	})
	d.emit(Event{Kind: "peers", Data: d.Peers()})
}

// mdnsLoop advertises and browses _chuanshu._tcp. Failures are
// non-fatal: UDP presence still works on flat LANs.
func (d *Daemon) mdnsLoop(ctx context.Context) {
	host, _ := os.Hostname()
	if !strings.HasSuffix(host, ".") {
		host += "."
	}
	var ips []net.IP
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	txt := []string{
		"peer=" + d.ID.MyPeerID(),
		"nick=" + d.Cfg.Nickname,
		"noise=" + base64.StdEncoding.EncodeToString(d.ID.NoisePublicBytes()),
	}
	svc, err := mdns.NewMDNSService(d.ID.MyPeerID(), "_chuanshu._tcp", "local.", host, d.Cfg.TCPPort, ips, txt)
	if err != nil {
		log.Printf("mdns service: %v", err)
		return
	}
	server, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		log.Printf("mdns server: %v", err)
		return
	}
	defer server.Shutdown()
	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		entries := make(chan *mdns.ServiceEntry, 8)
		done := make(chan struct{})
		go func() {
			for e := range entries {
				d.handleMDNSEntry(e)
			}
			close(done)
		}()
		_ = mdns.Query(&mdns.QueryParam{
			Service: "_chuanshu._tcp", Domain: "local.",
			Timeout: 4 * time.Second, Entries: entries,
		})
		close(entries)
		<-done
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (d *Daemon) handleMDNSEntry(e *mdns.ServiceEntry) {
	txt := map[string]string{}
	for _, t := range e.InfoFields {
		if k, v, ok := strings.Cut(t, "="); ok {
			txt[k] = v
		}
	}
	peerID := txt["peer"]
	if peerID == "" || peerID == d.ID.MyPeerID() {
		return
	}
	ip := e.AddrV4
	if ip == nil {
		ip = e.AddrV6
	}
	if ip == nil {
		return
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(e.Port))
	_, _ = d.Store.UpsertPeer(&store.PeerRecord{
		PeerID: peerID, Nickname: txt["nick"], NoisePub: txt["noise"],
		Addr: addr, LastSeen: time.Now(),
	})
	d.emit(Event{Kind: "peers", Data: d.Peers()})
}

// AddManualPeer registers and connects to a peer by address. This is
// the fallback when broadcast/multicast is blocked, and how tests
// wire two daemons together.
func (d *Daemon) AddManualPeer(addr string) (*store.PeerRecord, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("地址格式应为 ip:端口: %w", err)
	}
	conn, err := net.DialTimeout("tcp", addr, 4*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	sess, err := wire.DialSession(conn, d.ID)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	d.registerConn(sess, true)
	peerID := identity.PeerID(sess.RemotePub)
	_, _ = d.Store.UpsertPeer(&store.PeerRecord{PeerID: peerID, Addr: addr, Manual: true, LastSeen: time.Now()})
	// Wait briefly for the hello so the nickname is known.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := d.Store.Peer(peerID); ok && rec.Nickname != "" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	rec, _ := d.Store.Peer(peerID)
	d.emit(Event{Kind: "peers", Data: d.Peers()})
	return rec, nil
}
