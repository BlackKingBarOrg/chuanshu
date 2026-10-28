package wire

import (
	"net"
	"testing"

	"chuanshu/internal/identity"
)

func TestNoiseSessionRoundTrip(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	idA, err := identity.LoadOrCreate(dirA)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := identity.LoadOrCreate(dirB)
	if err != nil {
		t.Fatal(err)
	}
	// Real TCP loopback (net.Pipe is synchronous and would need a
	// concurrent reader for every frame; TCP matches production).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct {
		sess *Session
		err  error
	}
	accepted := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- result{nil, err}
			return
		}
		s, err := AcceptSession(conn, idB)
		accepted <- result{s, err}
	}()
	c1, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sa, err := DialSession(c1, idA)
	if err != nil {
		t.Fatalf("dial session: %v", err)
	}
	rb := <-accepted
	if rb.err != nil {
		t.Fatalf("accept session: %v", rb.err)
	}
	sb := rb.sess
	if got := identity.PeerID(sa.RemotePub); got != idB.MyPeerID() {
		t.Fatalf("initiator sees peer %s, want %s", got, idB.MyPeerID())
	}
	if got := identity.PeerID(sb.RemotePub); got != idA.MyPeerID() {
		t.Fatalf("responder sees peer %s, want %s", got, idA.MyPeerID())
	}

	// Control message A -> B.
	if err := sa.SendMessage(Message{Type: TypeChat, Text: "加密你好"}); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := sb.Recv()
	if err != nil || msg == nil || msg.Text != "加密你好" {
		t.Fatalf("B recv = %+v, %v", msg, err)
	}
	// Reply B -> A exercises the reverse cipher state.
	if err := sb.SendMessage(Message{Type: TypeChat, Text: "收到"}); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err = sa.Recv()
	if err != nil || msg == nil || msg.Text != "收到" {
		t.Fatalf("A recv = %+v, %v", msg, err)
	}
	// File chunk frame.
	if err := sa.SendChunk(ChunkHeader{TransferID: "t1", Offset: 7}, []byte("chunkdata")); err != nil {
		t.Fatal(err)
	}
	_, hdr, data, err := sb.Recv()
	if err != nil || hdr == nil || hdr.TransferID != "t1" || hdr.Offset != 7 || string(data) != "chunkdata" {
		t.Fatalf("chunk = %+v %q, %v", hdr, data, err)
	}
	_ = sa.Close()
	_ = sb.Close()
}
