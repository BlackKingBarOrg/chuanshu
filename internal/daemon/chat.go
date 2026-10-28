package daemon

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"chuanshu/internal/identity"
	"chuanshu/internal/store"
	"chuanshu/internal/wire"
)

// readLoop dispatches everything arriving on one peer session.
func (d *Daemon) readLoop(pc *peerConn) {
	defer func() {
		d.failIncomingFrom(pc.peerID)
		d.dropConn(pc.peerID)
	}()
	for {
		msg, chunkHdr, chunkData, err := pc.sess.Recv()
		if err != nil {
			return
		}
		if chunkHdr != nil {
			d.handleChunk(pc, chunkHdr, chunkData)
			continue
		}
		if msg == nil {
			continue
		}
		d.touchPeer(pc.peerID, "", "")
		switch msg.Type {
		case wire.TypeHello:
			d.handleHello(pc, msg)
		case wire.TypeChat:
			d.handleChat(pc, msg)
		case wire.TypeAckDelivered, wire.TypeAckRead:
			status := "delivered"
			if msg.Type == wire.TypeAckRead {
				status = "read"
			}
			_ = d.Store.UpdateStatus(msg.ID, status)
			d.notifyAck(msg.ID, status)
			d.emit(Event{Kind: "message", Data: map[string]any{"id": msg.ID, "status": status}})
		case wire.TypeFileOffer:
			d.handleOffer(pc, msg)
		case wire.TypeFileAccept:
			d.notifyOfferResult(msg.TransferID, offerResult{accept: true, offset: msg.Offset})
		case wire.TypeFileReject:
			d.notifyOfferResult(msg.TransferID, offerResult{accept: false, reason: msg.Reason})
		case wire.TypeFileDone:
			d.notifyDone(msg.TransferID, nil)
		case wire.TypeFileError:
			d.notifyDone(msg.TransferID, fmt.Errorf("%s", msg.Reason))
			d.failIncoming(msg.TransferID, msg.Reason)
		case wire.TypePing:
			_ = pc.sess.SendMessage(wire.Message{Type: wire.TypePong, ID: msg.ID})
		case wire.TypePong:
		}
	}
}

func (d *Daemon) handleHello(pc *peerConn, msg *wire.Message) {
	d.mu.Lock()
	pc.nickname = msg.Nickname
	d.mu.Unlock()
	rec := &store.PeerRecord{
		PeerID: pc.peerID, Nickname: msg.Nickname, LastSeen: time.Now(),
		NoisePub: base64.StdEncoding.EncodeToString(pc.sess.RemotePub),
	}
	if msg.SignPublic != "" {
		if b, err := base64.StdEncoding.DecodeString(msg.SignPublic); err == nil && len(b) > 0 {
			rec.SignPub = msg.SignPublic
		}
	}
	_, _ = d.Store.UpsertPeer(rec)
	d.emit(Event{Kind: "peers", Data: d.Peers()})
}

func (d *Daemon) handleChat(pc *peerConn, msg *wire.Message) {
	conv := pc.peerID
	if msg.Broadcast {
		conv = store.BroadcastConv
	}
	name := pc.nickname
	if rec, ok := d.Store.Peer(pc.peerID); ok && rec.Nickname != "" {
		name = rec.Nickname
	}
	cm := &store.ChatMessage{
		ID: msg.ID, Conv: conv, FromID: pc.peerID, FromName: name,
		Text: msg.Text, TS: msg.TS, Status: "received", Broadcast: msg.Broadcast,
	}
	_ = d.Store.AddMessage(cm)
	// Delivery ack rides back immediately: the daemon has it,
	// regardless of whether the human has looked yet.
	_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeAckDelivered, ID: msg.ID})
	d.emit(Event{Kind: "message", Data: cm})
	label := msg.Text
	if len([]rune(label)) > 40 {
		label = string([]rune(label)[:40]) + "…"
	}
	d.notify(name, label)
}

func (d *Daemon) notifyAck(id, status string) {
	d.mu.Lock()
	ch, ok := d.pendingAck[id]
	if ok {
		delete(d.pendingAck, id)
	}
	d.mu.Unlock()
	if ok {
		select {
		case ch <- status:
		default:
		}
	}
}

// SendText sends one chat message. Broadcast fans out to every
// online peer. Offline recipients fail immediately.
func (d *Daemon) SendText(query, text string) (*store.ChatMessage, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("消息为空")
	}
	peer, err := d.ResolvePeer(query)
	if err != nil {
		return nil, err
	}
	if peer.PeerID == store.BroadcastConv {
		return d.sendBroadcast(text)
	}
	return d.sendTextTo(peer, text, false)
}

func (d *Daemon) sendBroadcast(text string) (*store.ChatMessage, error) {
	cm := &store.ChatMessage{
		ID: wire.NewID(), Conv: store.BroadcastConv, FromID: d.ID.MyPeerID(),
		FromName: d.Cfg.Nickname, Mine: true, Text: text, TS: time.Now(),
		Status: "sending", Broadcast: true,
	}
	_ = d.Store.AddMessage(cm)
	sent := 0
	for _, p := range d.Peers() {
		if !p.Online {
			continue
		}
		if _, err := d.sendTextTo(p, text, true, cm.ID); err == nil {
			sent++
		}
	}
	if sent == 0 {
		_ = d.Store.UpdateStatus(cm.ID, "failed")
		cm.Status = "failed"
		d.emit(Event{Kind: "message", Data: cm})
		return cm, fmt.Errorf("没有在线的同事，广播未发出")
	}
	_ = d.Store.UpdateStatus(cm.ID, "delivered")
	cm.Status = "delivered"
	d.emit(Event{Kind: "message", Data: cm})
	return cm, nil
}

func (d *Daemon) sendTextTo(peer *store.PeerRecord, text string, broadcast bool, reuseID ...string) (*store.ChatMessage, error) {
	cm := &store.ChatMessage{
		ID: wire.NewID(), Conv: peer.PeerID, FromID: d.ID.MyPeerID(),
		FromName: d.Cfg.Nickname, Mine: true, Text: text, TS: time.Now(),
		Status: "sending", Broadcast: broadcast,
	}
	if broadcast {
		cm.Conv = store.BroadcastConv
		if len(reuseID) > 0 {
			cm.ID = reuseID[0]
		}
	} else {
		_ = d.Store.AddMessage(cm)
	}
	pc, err := d.ensureConn(peer.PeerID)
	if err != nil {
		if !broadcast {
			_ = d.Store.UpdateStatus(cm.ID, "failed")
			cm.Status = "failed"
			d.emit(Event{Kind: "message", Data: cm})
		}
		return cm, err
	}
	ackCh := make(chan string, 2)
	d.mu.Lock()
	d.pendingAck[cm.ID] = ackCh
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pendingAck, cm.ID)
		d.mu.Unlock()
	}()
	err = pc.sess.SendMessage(wire.Message{
		Type: wire.TypeChat, ID: cm.ID, TS: cm.TS, Text: text, Broadcast: broadcast,
	})
	if err != nil {
		if !broadcast {
			_ = d.Store.UpdateStatus(cm.ID, "failed")
			cm.Status = "failed"
		}
		return cm, fmt.Errorf("发送失败: %w", err)
	}
	// Wait briefly for the delivery ack so callers can report truth.
	select {
	case status := <-ackCh:
		if !broadcast {
			_ = d.Store.UpdateStatus(cm.ID, status)
			cm.Status = status
			d.emit(Event{Kind: "message", Data: cm})
		}
		return cm, nil
	case <-time.After(5 * time.Second):
		if !broadcast {
			_ = d.Store.UpdateStatus(cm.ID, "sent")
			cm.Status = "sent"
			d.emit(Event{Kind: "message", Data: cm})
		}
		return cm, nil
	}
}

// MarkRead marks a conversation read locally and acks senders.
func (d *Daemon) MarkRead(conv string) error {
	ids, err := d.Store.MarkRead(conv)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	// Group acks by original sender.
	byPeer := map[string][]string{}
	for _, m := range d.Store.Messages(conv) {
		for _, id := range ids {
			if m.ID == id {
				byPeer[m.FromID] = append(byPeer[m.FromID], id)
			}
		}
	}
	for peerID, list := range byPeer {
		if pc, ok := d.connFor(peerID); ok {
			for _, id := range list {
				_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeAckRead, ID: id})
			}
		}
	}
	d.emit(Event{Kind: "message", Data: map[string]any{"conv": conv, "read": true}})
	return nil
}

// VerifyPeer marks a peer verified after the human compared codes.
func (d *Daemon) VerifyPeer(query string) (string, error) {
	peer, err := d.ResolvePeer(query)
	if err != nil {
		return "", err
	}
	pub, err := base64.StdEncoding.DecodeString(peer.NoisePub)
	if err != nil {
		return "", err
	}
	if err := d.Store.SetVerified(peer.PeerID); err != nil {
		return "", err
	}
	d.emit(Event{Kind: "peers", Data: d.Peers()})
	return identity.Code(pub), nil
}

// PeerCode returns the verification code for a peer.
func (d *Daemon) PeerCode(query string) (string, error) {
	peer, err := d.ResolvePeer(query)
	if err != nil {
		return "", err
	}
	pub, err := base64.StdEncoding.DecodeString(peer.NoisePub)
	if err != nil {
		return "", err
	}
	return identity.Code(pub), nil
}
