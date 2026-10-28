// Package store persists peers and chat history as JSON documents
// on local disk. The scale target is a handful of people, so whole-
// file rewrites under a mutex are deliberate and sufficient.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// BroadcastConv is the conversation key for the all-hands channel.
const BroadcastConv = "#all"

// PeerRecord is a known machine.
type PeerRecord struct {
	PeerID   string    `json:"peer_id"`
	Nickname string    `json:"nickname"`
	NoisePub string    `json:"noise_pub"`
	SignPub  string    `json:"sign_pub,omitempty"`
	Addr     string    `json:"addr,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	Verified bool      `json:"verified,omitempty"`
	Manual   bool      `json:"manual,omitempty"`
	Online   bool      `json:"online,omitempty"`
}

// Attachment is a file linked from a chat message.
type Attachment struct {
	TransferID string `json:"transfer_id"`
	Name       string `json:"name"`
	RelPath    string `json:"rel_path,omitempty"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256,omitempty"`
	LocalPath  string `json:"local_path,omitempty"`
	IsImage    bool   `json:"is_image,omitempty"`
}

// ChatMessage is one history entry.
type ChatMessage struct {
	ID         string      `json:"id"`
	Conv       string      `json:"conv"`
	FromID     string      `json:"from_id"`
	FromName   string      `json:"from_name"`
	Mine       bool        `json:"mine"`
	Text       string      `json:"text,omitempty"`
	TS         time.Time   `json:"ts"`
	Status     string      `json:"status"` // sending|sent|delivered|read|failed|received
	Broadcast  bool        `json:"broadcast,omitempty"`
	Attachment *Attachment `json:"attachment,omitempty"`
}

// Store is the persistent state handle.
type Store struct {
	dir      string
	mu       sync.Mutex
	peers    map[string]*PeerRecord
	messages []*ChatMessage
}

// Open loads (or initializes) the store under dir.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, peers: map[string]*PeerRecord{}}
	if raw, err := os.ReadFile(filepath.Join(dir, "peers.json")); err == nil {
		var list []*PeerRecord
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		for _, p := range list {
			s.peers[p.PeerID] = p
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "messages.json")); err == nil {
		if err := json.Unmarshal(raw, &s.messages); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func writeAtomic(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) savePeersLocked() error {
	list := make([]*PeerRecord, 0, len(s.peers))
	for _, p := range s.peers {
		cp := *p
		cp.Online = false
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Nickname < list[j].Nickname })
	return writeAtomic(filepath.Join(s.dir, "peers.json"), list)
}

func (s *Store) saveMessagesLocked() error {
	return writeAtomic(filepath.Join(s.dir, "messages.json"), s.messages)
}

// UpsertPeer inserts or updates a peer and persists.
func (s *Store) UpsertPeer(p *PeerRecord) (*PeerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.peers[p.PeerID]; ok {
		if p.Nickname != "" {
			old.Nickname = p.Nickname
		}
		if p.NoisePub != "" {
			old.NoisePub = p.NoisePub
		}
		if p.SignPub != "" {
			old.SignPub = p.SignPub
		}
		if p.Addr != "" {
			old.Addr = p.Addr
		}
		if !p.LastSeen.IsZero() {
			old.LastSeen = p.LastSeen
		}
		old.Manual = old.Manual || p.Manual
		p = old
	} else {
		cp := *p
		s.peers[p.PeerID] = &cp
		p = &cp
	}
	cp := *p
	return &cp, s.savePeersLocked()
}

// SetVerified marks a peer's key as human-verified.
func (s *Store) SetVerified(peerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[peerID]
	if !ok {
		return os.ErrNotExist
	}
	p.Verified = true
	return s.savePeersLocked()
}

// Peer returns one peer by id.
func (s *Store) Peer(peerID string) (*PeerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[peerID]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// Peers returns all peers sorted by nickname.
func (s *Store) Peers() []*PeerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*PeerRecord, 0, len(s.peers))
	for _, p := range s.peers {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nickname < out[j].Nickname })
	return out
}

// Resolve finds a peer by exact id, id prefix, or nickname.
func (s *Store) Resolve(query string) (*PeerRecord, bool) {
	if p, ok := s.Peer(query); ok {
		return p, true
	}
	var match *PeerRecord
	for _, p := range s.Peers() {
		if p.Nickname == query || (len(query) >= 4 && len(p.PeerID) > len(query) && p.PeerID[:len(query)] == query) {
			if match != nil {
				return nil, false // ambiguous
			}
			cp := *p
			match = &cp
		}
	}
	return match, match != nil
}

// AddMessage appends a message and persists.
func (s *Store) AddMessage(m *ChatMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *m
	s.messages = append(s.messages, &cp)
	return s.saveMessagesLocked()
}

// UpdateStatus changes one message's status.
func (s *Store) UpdateStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.ID == id {
			m.Status = status
			return s.saveMessagesLocked()
		}
	}
	return nil
}

// UpdateAttachmentPath records where a received file landed.
func (s *Store) UpdateAttachmentPath(transferID, localPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.messages {
		if m.Attachment != nil && m.Attachment.TransferID == transferID {
			m.Attachment.LocalPath = localPath
			return s.saveMessagesLocked()
		}
	}
	return nil
}

// Messages returns history for one conversation in order.
func (s *Store) Messages(conv string) []*ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ChatMessage
	for _, m := range s.messages {
		if m.Conv == conv {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out
}

// UnreadCounts returns unread received counts per conversation.
func (s *Store) UnreadCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, m := range s.messages {
		if !m.Mine && m.Status == "received" {
			out[m.Conv]++
		}
	}
	return out
}

// MarkRead flips received messages in a conversation to read and
// returns their ids so the caller can send read acks.
func (s *Store) MarkRead(conv string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, m := range s.messages {
		if m.Conv == conv && !m.Mine && m.Status == "received" {
			m.Status = "read"
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return ids, s.saveMessagesLocked()
}
