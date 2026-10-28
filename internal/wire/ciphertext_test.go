package wire

import (
	"bytes"
	"net"
	"sync"
	"testing"

	"chuanshu/internal/identity"
)

// recordConn captures everything written to the connection, i.e.
// exactly what a LAN sniffer would see.
type recordConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *recordConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.buf.Write(b)
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *recordConn) captured() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte{}, c.buf.Bytes()...)
}

func TestWireCarriesOnlyCiphertext(t *testing.T) {
	idA, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idB, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *Session, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		s, err := AcceptSession(conn, idB)
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- s
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	wireConn := &recordConn{Conn: raw}
	sa, err := DialSession(wireConn, idA)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sb := <-accepted
	if sb == nil {
		t.Fatal("accept failed")
	}
	secret := "绝密-薪资表-不要外传"
	if err := sa.SendMessage(Message{Type: TypeChat, Text: secret}); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := sb.Recv()
	if err != nil || msg == nil || msg.Text != secret {
		t.Fatalf("recv = %+v, %v", msg, err)
	}
	seen := wireConn.captured()
	if len(seen) == 0 {
		t.Fatal("nothing captured on the wire")
	}
	if bytes.Contains(seen, []byte(secret)) {
		t.Fatal("plaintext found on the wire: chat is NOT encrypted in transit")
	}
	if bytes.Contains(seen, []byte("chat")) && bytes.Contains(seen, []byte("type")) {
		t.Fatal("JSON control structure visible on the wire")
	}
	_ = sa.Close()
	_ = sb.Close()
}
