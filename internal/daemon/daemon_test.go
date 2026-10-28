package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"chuanshu/internal/appcfg"
	"chuanshu/internal/store"
)

func TestSignedAnnounceDiscovery(t *testing.T) {
	t.Setenv("CHUANSHU_DISABLE_MDNS", "1")
	carol := startDaemon(t, "Carol")
	dave := startDaemon(t, "Dave")

	// A genuine signed announce from Carol lands in Dave's roster
	// with Carol's TCP address derived from the sender.
	ann := carol.d.myAnnounce()
	dave.d.handleAnnounce(ann, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9999})
	rec, ok := dave.d.Store.Resolve("Carol")
	if !ok {
		t.Fatal("signed announce did not register Carol")
	}
	wantAddr := fmt.Sprintf("127.0.0.1:%d", carol.d.Cfg.TCPPort)
	if rec.Addr != wantAddr {
		t.Fatalf("announced addr = %q, want %q", rec.Addr, wantAddr)
	}

	// A tampered announce (nickname changed after signing) must be
	// rejected: identity is the key, not the claimed name.
	forged := carol.d.myAnnounce()
	forged.Nickname = "Mallory"
	dave.d.handleAnnounce(forged, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9999})
	if _, ok := dave.d.Store.Resolve("Mallory"); ok {
		t.Fatal("forged announce was accepted")
	}
	if rec, _ := dave.d.Store.Peer(carol.d.ID.MyPeerID()); rec.Nickname != "Carol" {
		t.Fatalf("roster nickname overwritten by forgery: %q", rec.Nickname)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type testDaemon struct {
	d      *Daemon
	cancel context.CancelFunc
	paths  appcfg.Paths
}

func startDaemon(t *testing.T, nickname string) *testDaemon {
	t.Helper()
	return startDaemonAt(t, t.TempDir(), nickname, freePort(t), freePort(t))
}

func startDaemonAt(t *testing.T, root, nickname string, tcpPort, udpPort int) *testDaemon {
	t.Helper()
	paths := appcfg.Paths{
		ConfigDir:   root,
		DataDir:     root,
		DownloadDir: filepath.Join(root, "downloads"),
		CacheDir:    filepath.Join(root, "cache"),
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := appcfg.Config{Nickname: nickname, TCPPort: tcpPort, DiscoveryUDPPort: udpPort}
	d, err := New(cfg, paths)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Start(ctx) }()
	// Wait for the TCP listener.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.TCPPort), 100*time.Millisecond); err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	td := &testDaemon{d: d, cancel: cancel, paths: paths}
	t.Cleanup(cancel)
	return td
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestUnreadSummaryAndFocus(t *testing.T) {
	t.Setenv("CHUANSHU_DISABLE_MDNS", "1")
	alice := startDaemon(t, "Alice")
	bob := startDaemon(t, "Bob")
	if _, err := alice.d.AddManualPeer(fmt.Sprintf("127.0.0.1:%d", bob.d.Cfg.TCPPort)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob knows alice", func() bool {
		_, ok := bob.d.Store.Resolve("Alice")
		return ok
	})
	if _, err := alice.d.SendText("Bob", "未读测试"); err != nil {
		t.Fatal(err)
	}
	aliceID := alice.d.ID.MyPeerID()
	waitFor(t, "bob unread summary", func() bool {
		sum := bob.d.UnreadSummary()
		return sum.Total == 1 && sum.LatestConv == aliceID && sum.LatestName == "Alice"
	})
	if err := bob.d.MarkRead(aliceID); err != nil {
		t.Fatal(err)
	}
	if sum := bob.d.UnreadSummary(); sum.Total != 0 {
		t.Fatalf("after MarkRead total = %d, want 0", sum.Total)
	}
	// Focus asks resolve nicknames to conversation ids.
	conv, err := bob.d.FocusConv("Alice")
	if err != nil || conv != aliceID {
		t.Fatalf("FocusConv = %q, %v; want %q", conv, err, aliceID)
	}
	// Window registration round-trips.
	bob.d.RegisterWindow("0xabc")
	if bob.d.Window() != "0xabc" {
		t.Fatal("window not registered")
	}
	bob.d.UnregisterWindow("0xabc")
	if bob.d.Window() != "" {
		t.Fatal("window not unregistered")
	}
	// Stop both daemons before TempDir cleanup races their writes.
	alice.cancel()
	bob.cancel()
	time.Sleep(150 * time.Millisecond)
}

func TestOfflineMessageResentWhenPeerReturns(t *testing.T) {
	t.Setenv("CHUANSHU_DISABLE_MDNS", "1")
	alice := startDaemon(t, "Alice")
	bobRoot := t.TempDir()
	bobTCP, bobUDP := freePort(t), freePort(t)
	bob := startDaemonAt(t, bobRoot, "Bob", bobTCP, bobUDP)
	bobID := bob.d.ID.MyPeerID()

	if _, err := alice.d.AddManualPeer(fmt.Sprintf("127.0.0.1:%d", bobTCP)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice knows bob", func() bool {
		_, ok := alice.d.Store.Resolve("Bob")
		return ok
	})

	// Bob goes offline; Alice's message must queue, not fail.
	bob.cancel()
	waitFor(t, "alice drops bob", func() bool {
		_, ok := alice.d.connFor(bobID)
		return !ok
	})
	time.Sleep(100 * time.Millisecond)
	cm, err := alice.d.SendText("Bob", "你回来后能看到这条")
	if err != nil {
		t.Fatalf("offline send: %v", err)
	}
	if cm.Status != "queued" {
		t.Fatalf("status = %q, want queued", cm.Status)
	}

	// Bob returns with the same identity and port: the queued
	// message must arrive automatically, exactly once.
	bob2 := startDaemonAt(t, bobRoot, "Bob", bobTCP, bobUDP)
	if bob2.d.ID.MyPeerID() != bobID {
		t.Fatal("restarted bob changed identity")
	}
	waitFor(t, "bob receives queued message after return", func() bool {
		n := 0
		for _, m := range bob2.d.Store.Messages(alice.d.ID.MyPeerID()) {
			if !m.Mine && m.Text == "你回来后能看到这条" {
				n++
			}
		}
		return n == 1
	})
	waitFor(t, "alice sees delivered", func() bool {
		for _, m := range alice.d.Store.Messages(bobID) {
			if m.ID == cm.ID && m.Status == "delivered" {
				return true
			}
		}
		return false
	})
	alice.cancel()
	bob2.cancel()
	time.Sleep(150 * time.Millisecond)
}

func TestTwoDaemonsChatAndFile(t *testing.T) {
	t.Setenv("CHUANSHU_DISABLE_MDNS", "1")
	alice := startDaemon(t, "Alice")
	bob := startDaemon(t, "Bob")

	if _, err := alice.d.AddManualPeer(fmt.Sprintf("127.0.0.1:%d", bob.d.Cfg.TCPPort)); err != nil {
		t.Fatalf("peer-add: %v", err)
	}
	waitFor(t, "bob sees alice nickname", func() bool {
		_, ok := bob.d.Store.Resolve("Alice")
		return ok
	})
	waitFor(t, "alice sees bob nickname", func() bool {
		_, ok := alice.d.Store.Resolve("Bob")
		return ok
	})

	// Encrypted chat A -> B and B -> A (exercises both Noise
	// transport directions).
	cm, err := alice.d.SendText("Bob", "你好，Bob")
	if err != nil {
		t.Fatalf("send A->B: %v", err)
	}
	if cm.Status != "delivered" {
		t.Fatalf("A->B status = %q, want delivered", cm.Status)
	}
	aliceID := alice.d.ID.MyPeerID()
	waitFor(t, "bob received chat", func() bool {
		return len(bob.d.Store.Messages(aliceID)) == 1
	})
	if _, err := bob.d.SendText("Alice", "你好，Alice"); err != nil {
		t.Fatalf("send B->A: %v", err)
	}
	bobID := bob.d.ID.MyPeerID()
	waitFor(t, "alice received reply", func() bool {
		n := 0
		for _, m := range alice.d.Store.Messages(bobID) {
			if !m.Mine {
				n++
			}
		}
		return n == 1
	})

	// Read ack flows back.
	if err := bob.d.MarkRead(aliceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sees read status", func() bool {
		for _, m := range alice.d.Store.Messages(bobID) {
			if m.Mine && m.Status == "read" {
				return true
			}
		}
		return false
	})

	// File transfer with explicit accept, then resume on resend.
	payload := make([]byte, 200_000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(alice.paths.CacheDir, "report.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(payload)

	sendErr := make(chan error, 1)
	go func() {
		_, err := alice.d.SendPath("Bob", src)
		sendErr <- err
	}()
	waitFor(t, "bob has pending offer", func() bool { return len(bob.d.Offers()) == 1 })
	offer := bob.d.Offers()[0]
	if err := bob.d.RespondOffer(offer.TransferID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("file send: %v", err)
	}
	target := filepath.Join(bob.paths.DownloadDir, "report.bin")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != wantSum {
		t.Fatal("received file hash mismatch")
	}

	// Resume: pre-seed a .part with the first bytes, resend, and
	// confirm integrity (receiver offsets into the partial file).
	src2 := filepath.Join(alice.paths.CacheDir, "resume.bin")
	if err := os.WriteFile(src2, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(bob.paths.DownloadDir, "resume.bin.part")
	if err := os.WriteFile(partPath, payload[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := alice.d.SendPath("Bob", src2)
		sendErr <- err
	}()
	waitFor(t, "bob has resume offer", func() bool { return len(bob.d.Offers()) == 1 })
	if err := bob.d.RespondOffer(bob.d.Offers()[0].TransferID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("resume send: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(bob.paths.DownloadDir, "resume.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if gotSum := sha256.Sum256(got); hex.EncodeToString(gotSum[:]) != hex.EncodeToString(wantSum[:]) {
		t.Fatal("resumed file hash mismatch")
	}
	_ = store.BroadcastConv

	// Offline queueing: a known peer that shut down gets the
	// message queued (not failed); it flushes when he returns.
	bob.cancel()
	waitFor(t, "alice drops bob connection", func() bool {
		_, ok := alice.d.connFor(bobID)
		return !ok
	})
	cm2, err := alice.d.SendText("Bob", "还在吗")
	if err != nil {
		t.Fatalf("send to offline known peer: %v", err)
	}
	if cm2.Status != "queued" {
		t.Fatalf("offline message status = %q, want queued", cm2.Status)
	}

	// Unknown peer still fails immediately (nobody to queue for).
	if _, err := alice.d.SendText("不存在的同事", "hi"); err == nil {
		t.Fatal("send to unknown peer succeeded, want failure")
	}
}
