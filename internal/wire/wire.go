// Package wire defines the on-the-wire protocol: Noise XX sessions
// over TCP, JSON control messages and binary file chunks carried
// inside the encrypted channel.
package wire

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"chuanshu/internal/identity"

	"github.com/flynn/noise"
)

// Prologue binds handshakes to this protocol and version.
const Prologue = "chuanshu/1"

// MaxChunk is the file chunk payload; Noise frames cap at 65535
// bytes including overhead, so chunks stay well below.
const MaxChunk = 32 * 1024

// Message types carried as JSON control frames.
const (
	TypeHello        = "hello"
	TypeChat         = "chat"
	TypeAckDelivered = "ack_delivered"
	TypeAckRead      = "ack_read"
	TypeFileOffer    = "file_offer"
	TypeFileAccept   = "file_accept"
	TypeFileReject   = "file_reject"
	TypeFileDone     = "file_done"
	TypeFileError    = "file_error"
	TypePing         = "ping"
	TypePong         = "pong"
)

// FileMeta describes one offered file.
type FileMeta struct {
	Name    string `json:"name"`
	RelPath string `json:"rel_path,omitempty"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	GroupID string `json:"group_id,omitempty"`
	IsImage bool   `json:"is_image,omitempty"`
}

// Message is a JSON control frame.
type Message struct {
	Type       string    `json:"type"`
	ID         string    `json:"id,omitempty"`
	TS         time.Time `json:"ts,omitempty"`
	Text       string    `json:"text,omitempty"`
	Broadcast  bool      `json:"broadcast,omitempty"`
	Nickname   string    `json:"nickname,omitempty"`
	SignPublic string    `json:"sign_public,omitempty"`
	TransferID string    `json:"transfer_id,omitempty"`
	File       *FileMeta `json:"file,omitempty"`
	Offset     int64     `json:"offset,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// NewID returns a random message/transfer id.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Frame kinds inside the encrypted stream.
const (
	FrameMessage byte = 'M'
	FrameChunk   byte = 'C'
)

// ChunkHeader precedes raw file bytes in a chunk frame.
type ChunkHeader struct {
	TransferID string `json:"transfer_id"`
	Offset     int64  `json:"offset"`
}

// Session is an established Noise transport over a TCP connection.
type Session struct {
	Conn      net.Conn
	send      *noise.CipherState
	recv      *noise.CipherState
	RemotePub []byte
	writeMu   sync.Mutex
}

func writeFrame(w io.Writer, b []byte) error {
	if len(b) > 65535 {
		return fmt.Errorf("frame too large: %d", len(b))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	return buf, err
}

func handshakeConfig(id *identity.Identity, initiator bool) (noise.Config, error) {
	kp, err := id.NoiseKeypair()
	if err != nil {
		return noise.Config{}, err
	}
	return noise.Config{
		CipherSuite:   identity.CipherSuite(),
		Pattern:       noise.HandshakeXX,
		Initiator:     initiator,
		StaticKeypair: kp,
		Prologue:      []byte(Prologue),
	}, nil
}

// DialSession performs the initiator side of Noise XX.
func DialSession(conn net.Conn, id *identity.Identity) (*Session, error) {
	cfg, err := handshakeConfig(id, true)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, err
	}
	msg, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, msg); err != nil {
		return nil, err
	}
	resp, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, resp); err != nil {
		return nil, err
	}
	msg, cs1, cs2, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, msg); err != nil {
		return nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, fmt.Errorf("handshake did not produce cipher states")
	}
	return &Session{Conn: conn, send: cs1, recv: cs2, RemotePub: hs.PeerStatic()}, nil
}

// AcceptSession performs the responder side of Noise XX.
func AcceptSession(conn net.Conn, id *identity.Identity) (*Session, error) {
	cfg, err := handshakeConfig(id, false)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return nil, err
	}
	msg, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := hs.ReadMessage(nil, msg); err != nil {
		return nil, err
	}
	out, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, out); err != nil {
		return nil, err
	}
	final, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	_, cs1, cs2, err := hs.ReadMessage(nil, final)
	if err != nil {
		return nil, err
	}
	if cs1 == nil || cs2 == nil {
		return nil, fmt.Errorf("handshake did not produce cipher states")
	}
	// Responder sends with the second cipher state.
	return &Session{Conn: conn, send: cs2, recv: cs1, RemotePub: hs.PeerStatic()}, nil
}

// SendMessage encrypts and sends one JSON control message.
func (s *Session) SendMessage(m Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.TS.IsZero() {
		m.TS = time.Now()
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.sendPayload(append([]byte{FrameMessage}, raw...))
}

// SendChunk encrypts and sends one file chunk.
func (s *Session) SendChunk(h ChunkHeader, data []byte) error {
	hdr, err := json.Marshal(h)
	if err != nil {
		return err
	}
	payload := make([]byte, 0, 1+2+len(hdr)+len(data))
	payload = append(payload, FrameChunk)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(hdr)))
	payload = append(payload, lb[:]...)
	payload = append(payload, hdr...)
	payload = append(payload, data...)
	return s.sendPayload(payload)
}

func (s *Session) sendPayload(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ct, err := s.send.Encrypt(nil, nil, p)
	if err != nil {
		return err
	}
	return writeFrame(s.Conn, ct)
}

// Recv decrypts the next frame. Exactly one of the return values is
// meaningful: a control message or a file chunk.
func (s *Session) Recv() (*Message, *ChunkHeader, []byte, error) {
	ct, err := readFrame(s.Conn)
	if err != nil {
		return nil, nil, nil, err
	}
	pt, err := s.recv.Decrypt(nil, nil, ct)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(pt) == 0 {
		return nil, nil, nil, fmt.Errorf("empty frame")
	}
	switch pt[0] {
	case FrameMessage:
		var m Message
		if err := json.Unmarshal(pt[1:], &m); err != nil {
			return nil, nil, nil, err
		}
		return &m, nil, nil, nil
	case FrameChunk:
		if len(pt) < 3 {
			return nil, nil, nil, fmt.Errorf("short chunk frame")
		}
		hl := int(binary.BigEndian.Uint16(pt[1:3]))
		if len(pt) < 3+hl {
			return nil, nil, nil, fmt.Errorf("short chunk header")
		}
		var h ChunkHeader
		if err := json.Unmarshal(pt[3:3+hl], &h); err != nil {
			return nil, nil, nil, err
		}
		return nil, &h, pt[3+hl:], nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown frame kind %q", pt[0])
	}
}

// Close closes the underlying connection.
func (s *Session) Close() error { return s.Conn.Close() }

// SanitizeName makes a received file name safe to join under a
// download directory. It fails closed: anything suspicious is
// rejected rather than rewritten.
func SanitizeName(name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	if path.Clean(name) != name {
		return "", fmt.Errorf("invalid file name %q", name)
	}
	return name, nil
}

// SanitizeRelPath cleans a folder-relative path element by element.
func SanitizeRelPath(rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if path.IsAbs(rel) || strings.Contains(rel, "\\") || strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if _, err := SanitizeName(p); err != nil {
			return "", err
		}
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid relative path %q", rel)
	}
	return cleaned, nil
}
