package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chuanshu/internal/store"
	"chuanshu/internal/wire"
)

type incomingOffer struct {
	TransferID string
	FromID     string
	FromName   string
	File       *wire.FileMeta
	Decide     chan bool
	Created    time.Time
}

type incomingTransfer struct {
	meta     *wire.FileMeta
	fromID   string
	target   string
	partPath string
	file     *os.File
	received int64
}

// PendingOffer is the IPC view of an undecided incoming file.
type PendingOffer struct {
	TransferID string         `json:"transfer_id"`
	FromID     string         `json:"from_id"`
	FromName   string         `json:"from_name"`
	File       *wire.FileMeta `json:"file"`
}

// Offers lists undecided incoming files.
func (d *Daemon) Offers() []PendingOffer {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []PendingOffer{}
	for _, o := range d.incomingOffers {
		out = append(out, PendingOffer{TransferID: o.TransferID, FromID: o.FromID, FromName: o.FromName, File: o.File})
	}
	return out
}

// RespondOffer accepts or rejects a pending incoming file.
func (d *Daemon) RespondOffer(transferID string, accept bool) error {
	d.mu.Lock()
	o, ok := d.incomingOffers[transferID]
	if ok {
		delete(d.incomingOffers, transferID)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("没有这个待确认文件")
	}
	o.Decide <- accept
	return nil
}

func (d *Daemon) notifyOfferResult(transferID string, r offerResult) {
	d.mu.Lock()
	ch, ok := d.pendingOffer[transferID]
	if ok {
		delete(d.pendingOffer, transferID)
	}
	d.mu.Unlock()
	if ok {
		ch <- r
	}
}

func (d *Daemon) notifyDone(transferID string, err error) {
	d.mu.Lock()
	ch, ok := d.pendingDone[transferID]
	if ok {
		delete(d.pendingDone, transferID)
	}
	d.mu.Unlock()
	if ok {
		ch <- err
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func isImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg":
		return true
	}
	return false
}

// SendPath sends a file or a whole folder to a peer. Folder members
// share a group id; the receiver confirms the group once.
func (d *Daemon) SendPath(query, path string) ([]*store.ChatMessage, error) {
	peer, err := d.ResolvePeer(query)
	if err != nil {
		return nil, err
	}
	if peer.PeerID == store.BroadcastConv {
		return nil, fmt.Errorf("广播频道不能发文件，请选一位同事")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	type item struct {
		full string
		rel  string
	}
	var items []item
	if info.IsDir() {
		base := filepath.Clean(path)
		err = filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil // symlinks skipped by policy
			}
			if fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(filepath.Dir(base), p)
			if err != nil {
				return err
			}
			items = append(items, item{full: p, rel: filepath.ToSlash(rel)})
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("文件夹是空的")
		}
	} else {
		items = []item{{full: path, rel: ""}}
	}
	groupID := ""
	if len(items) > 1 {
		groupID = wire.NewID()
	}
	var out []*store.ChatMessage
	for _, it := range items {
		cm, err := d.sendOneFile(peer, it.full, it.rel, groupID)
		if err != nil {
			return out, err
		}
		out = append(out, cm)
	}
	return out, nil
}

func (d *Daemon) sendOneFile(peer *store.PeerRecord, fullPath, rel, groupID string) (*store.ChatMessage, error) {
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}
	sum, err := hashFile(fullPath)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(fullPath)
	relPath := ""
	if rel != "" && rel != name {
		relPath = filepath.ToSlash(filepath.Dir(rel))
		if relPath == "." {
			relPath = ""
		}
	}
	transferID := wire.NewID()
	meta := &wire.FileMeta{
		Name: name, RelPath: relPath, Size: info.Size(), SHA256: sum,
		GroupID: groupID, IsImage: isImage(name),
	}
	cm := &store.ChatMessage{
		ID: wire.NewID(), Conv: peer.PeerID, FromID: d.ID.MyPeerID(),
		FromName: d.Cfg.Nickname, Mine: true, TS: time.Now(), Status: "sending",
		Attachment: &store.Attachment{
			TransferID: transferID, Name: name, RelPath: relPath,
			Size: info.Size(), SHA256: sum, LocalPath: fullPath, IsImage: meta.IsImage,
		},
	}
	_ = d.Store.AddMessage(cm)
	d.emit(Event{Kind: "message", Data: cm})

	fail := func(err error) (*store.ChatMessage, error) {
		_ = d.Store.UpdateStatus(cm.ID, "failed")
		cm.Status = "failed"
		d.emit(Event{Kind: "message", Data: cm})
		return cm, err
	}

	pc, err := d.ensureConn(peer.PeerID)
	if err != nil {
		return fail(err)
	}
	resultCh := make(chan offerResult, 1)
	doneCh := make(chan error, 1)
	d.mu.Lock()
	d.pendingOffer[transferID] = resultCh
	d.pendingDone[transferID] = doneCh
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pendingOffer, transferID)
		delete(d.pendingDone, transferID)
		d.mu.Unlock()
	}()

	if err := pc.sess.SendMessage(wire.Message{
		Type: wire.TypeFileOffer, TransferID: transferID, File: meta,
	}); err != nil {
		return fail(fmt.Errorf("发送文件请求失败: %w", err))
	}
	var offset int64
	select {
	case r := <-resultCh:
		if !r.accept {
			reason := r.reason
			if reason == "" {
				reason = "对方拒绝了文件"
			}
			return fail(fmt.Errorf("%s", reason))
		}
		offset = r.offset
	case <-time.After(2 * time.Minute):
		return fail(fmt.Errorf("对方长时间未确认，文件未发送"))
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return fail(err)
		}
	}
	buf := make([]byte, wire.MaxChunk)
	sent := offset
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := pc.sess.SendChunk(wire.ChunkHeader{TransferID: transferID, Offset: sent}, buf[:n]); err != nil {
				return fail(fmt.Errorf("传输中断: %w", err))
			}
			sent += int64(n)
			d.emit(Event{Kind: "transfer", Data: map[string]any{
				"transfer_id": transferID, "sent": sent, "size": info.Size(), "outgoing": true,
			}})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(rerr)
		}
	}
	select {
	case err := <-doneCh:
		if err != nil {
			return fail(fmt.Errorf("对方校验失败: %w", err))
		}
	case <-time.After(60 * time.Second):
		return fail(fmt.Errorf("等待对方确认超时"))
	}
	_ = d.Store.UpdateStatus(cm.ID, "delivered")
	cm.Status = "delivered"
	d.emit(Event{Kind: "message", Data: cm})
	return cm, nil
}

// handleOffer decides (or asks the human) and arms chunk reception.
func (d *Daemon) handleOffer(pc *peerConn, msg *wire.Message) {
	meta := msg.File
	if meta == nil {
		return
	}
	if _, err := wire.SanitizeName(meta.Name); err != nil {
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileReject, TransferID: msg.TransferID, Reason: "文件名不合法"})
		return
	}
	if _, err := wire.SanitizeRelPath(meta.RelPath); err != nil {
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileReject, TransferID: msg.TransferID, Reason: "路径不合法"})
		return
	}
	fromName := pc.nickname
	groupKey := ""
	if meta.GroupID != "" {
		groupKey = pc.peerID + "/" + meta.GroupID
	}
	d.mu.Lock()
	autoAccept := groupKey != "" && d.acceptedGroups[groupKey]
	d.mu.Unlock()
	if autoAccept {
		d.beginReceive(pc, msg, true)
		return
	}
	offer := &incomingOffer{
		TransferID: msg.TransferID, FromID: pc.peerID, FromName: fromName,
		File: meta, Decide: make(chan bool, 1), Created: time.Now(),
	}
	d.mu.Lock()
	d.incomingOffers[msg.TransferID] = offer
	d.mu.Unlock()
	d.emit(Event{Kind: "offer", Data: PendingOffer{TransferID: msg.TransferID, FromID: pc.peerID, FromName: fromName, File: meta}})
	d.notify(fmt.Sprintf("%s 发来文件", fromName), fmt.Sprintf("%s (%s) — 打开传书确认接收", meta.Name, humanSize(meta.Size)))
	go func() {
		select {
		case accept := <-offer.Decide:
			if meta.GroupID != "" && accept {
				d.mu.Lock()
				d.acceptedGroups[groupKey] = true
				d.mu.Unlock()
			}
			if !accept {
				_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileReject, TransferID: msg.TransferID, Reason: "对方拒绝接收"})
				return
			}
			d.beginReceive(pc, msg, false)
		case <-time.After(2 * time.Minute):
			d.mu.Lock()
			delete(d.incomingOffers, msg.TransferID)
			d.mu.Unlock()
		}
	}()
}

// beginReceive computes the resume offset from any .part file and
// tells the sender where to start.
func (d *Daemon) beginReceive(pc *peerConn, msg *wire.Message, auto bool) {
	meta := msg.File
	downloadDir := d.Cfg.EffectiveDownloadDir(d.Paths)
	target := filepath.Join(downloadDir, filepath.FromSlash(meta.RelPath), meta.Name)
	partPath := target + ".part"
	var offset int64
	if st, err := os.Stat(partPath); err == nil {
		if st.Size() <= meta.Size {
			offset = st.Size()
		} else {
			_ = os.Remove(partPath)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileReject, TransferID: msg.TransferID, Reason: "无法创建下载目录"})
		return
	}
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileReject, TransferID: msg.TransferID, Reason: "无法写入文件"})
		return
	}
	d.mu.Lock()
	d.incoming[msg.TransferID] = &incomingTransfer{
		meta: meta, fromID: pc.peerID, target: target, partPath: partPath, file: f, received: offset,
	}
	d.mu.Unlock()
	_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileAccept, TransferID: msg.TransferID, Offset: offset})
	if offset >= meta.Size {
		// Nothing left to transfer (zero-byte file, or a previous
		// attempt died after the last byte but before finalize).
		go d.finishReceive(pc, msg.TransferID)
	}
}

// failIncomingFrom closes any in-flight receives from a peer that
// went away, keeping .part files for a later resume.
func (d *Daemon) failIncomingFrom(peerID string) {
	d.mu.Lock()
	var ids []string
	for id, it := range d.incoming {
		if it.fromID == peerID {
			ids = append(ids, id)
		}
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.failIncoming(id, "对方连接断开")
	}
}

func (d *Daemon) handleChunk(pc *peerConn, h *wire.ChunkHeader, data []byte) {
	d.mu.Lock()
	it, ok := d.incoming[h.TransferID]
	d.mu.Unlock()
	if !ok {
		return
	}
	if h.Offset != it.received {
		d.failIncoming(h.TransferID, "分片偏移对不上")
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileError, TransferID: h.TransferID, Reason: "分片偏移对不上"})
		return
	}
	if _, err := it.file.WriteAt(data, h.Offset); err != nil {
		d.failIncoming(h.TransferID, "写入失败")
		return
	}
	it.received += int64(len(data))
	d.emit(Event{Kind: "transfer", Data: map[string]any{
		"transfer_id": h.TransferID, "sent": it.received, "size": it.meta.Size, "outgoing": false,
	}})
	if it.received >= it.meta.Size {
		d.finishReceive(pc, h.TransferID)
	}
}

func (d *Daemon) finishReceive(pc *peerConn, transferID string) {
	d.mu.Lock()
	it, ok := d.incoming[transferID]
	if ok {
		delete(d.incoming, transferID)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	_ = it.file.Close()
	sum, err := hashFile(it.partPath)
	if err != nil || sum != it.meta.SHA256 {
		_ = os.Remove(it.partPath)
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileError, TransferID: transferID, Reason: "文件完整性校验失败"})
		d.emit(Event{Kind: "notice", Data: map[string]any{"text": "收到文件校验失败，已删除: " + it.meta.Name}})
		return
	}
	final := uniquePath(it.target)
	if err := os.Rename(it.partPath, final); err != nil {
		_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileError, TransferID: transferID, Reason: "保存文件失败"})
		return
	}
	conv := it.fromID
	cm := &store.ChatMessage{
		ID: wire.NewID(), Conv: conv, FromID: it.fromID, FromName: pc.nickname,
		TS: time.Now(), Status: "received",
		Attachment: &store.Attachment{
			TransferID: transferID, Name: it.meta.Name, RelPath: it.meta.RelPath,
			Size: it.meta.Size, SHA256: it.meta.SHA256, LocalPath: final, IsImage: it.meta.IsImage,
		},
	}
	_ = d.Store.AddMessage(cm)
	_ = pc.sess.SendMessage(wire.Message{Type: wire.TypeFileDone, TransferID: transferID})
	d.emit(Event{Kind: "message", Data: cm})
	d.emitUnread()
	d.notify("文件接收完成", filepath.Base(final))
}

func (d *Daemon) failIncoming(transferID, reason string) {
	d.mu.Lock()
	it, ok := d.incoming[transferID]
	if ok {
		delete(d.incoming, transferID)
	}
	d.mu.Unlock()
	if ok {
		_ = it.file.Close()
		// Keep the .part file: a later resend resumes from it.
	}
	d.emit(Event{Kind: "notice", Data: map[string]any{"text": "文件接收中断（可续传）: " + reason}})
}

// uniquePath avoids overwriting an existing file.
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
