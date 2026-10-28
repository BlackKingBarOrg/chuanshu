package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"chuanshu/internal/store"
)

type ipcRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type ipcResponse struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

// serveIPC listens on the unix socket until ctx ends.
func (d *Daemon) serveIPC(ctx context.Context) error {
	sock := d.Paths.SocketPath()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("ipc listen: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(sock)
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.handleIPCConn(conn)
		}
	}()
	return nil
}

func (d *Daemon) handleIPCConn(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(conn)
	for sc.Scan() {
		var req ipcRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Method == "subscribe" {
			_ = enc.Encode(ipcResponse{ID: req.ID, OK: true, Result: d.Me()})
			ch := d.subscribe()
			defer d.unsubscribe(ch)
			for e := range ch {
				if err := enc.Encode(map[string]any{"event": e}); err != nil {
					return
				}
			}
			return
		}
		resp := d.dispatch(req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (d *Daemon) dispatch(req ipcRequest) ipcResponse {
	ok := func(v any) ipcResponse { return ipcResponse{ID: req.ID, OK: true, Result: v} }
	fail := func(err error) ipcResponse { return ipcResponse{ID: req.ID, OK: false, Error: err.Error()} }
	var p struct {
		Peer       string `json:"peer"`
		Text       string `json:"text"`
		Path       string `json:"path"`
		Addr       string `json:"addr"`
		Conv       string `json:"conv"`
		TransferID string `json:"transfer_id"`
		Accept     bool   `json:"accept"`
		Address    string `json:"address"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &p)
	}
	switch req.Method {
	case "me":
		return ok(d.Me())
	case "unread":
		return ok(d.UnreadSummary())
	case "register_window":
		d.RegisterWindow(p.Address)
		return ok(true)
	case "unregister_window":
		d.UnregisterWindow(p.Address)
		return ok(true)
	case "window":
		return ok(map[string]any{"address": d.Window()})
	case "focus_conv":
		conv := p.Conv
		if conv == "" {
			conv = p.Peer
		}
		resolved, err := d.FocusConv(conv)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"conv": resolved})
	case "peers":
		return ok(d.Peers())
	case "history":
		conv := p.Conv
		if conv == "" {
			conv = p.Peer
		}
		if conv != store.BroadcastConv {
			if peer, err := d.ResolvePeer(conv); err == nil {
				conv = peer.PeerID
			}
		}
		return ok(map[string]any{"messages": d.Store.Messages(conv), "unread": d.Store.UnreadCounts()})
	case "send_text":
		cm, err := d.SendText(p.Peer, p.Text)
		if err != nil {
			if cm != nil {
				resp := fail(err)
				resp.Result = cm
				return resp
			}
			return fail(err)
		}
		return ok(cm)
	case "send_file":
		msgs, err := d.SendPath(p.Peer, p.Path)
		if err != nil {
			return fail(err)
		}
		return ok(msgs)
	case "offers":
		return ok(d.Offers())
	case "respond_offer":
		if err := d.RespondOffer(p.TransferID, p.Accept); err != nil {
			return fail(err)
		}
		return ok(true)
	case "peer_add":
		rec, err := d.AddManualPeer(p.Addr)
		if err != nil {
			return fail(err)
		}
		return ok(rec)
	case "mark_read":
		conv := p.Conv
		if conv == "" {
			conv = p.Peer
		}
		if conv != store.BroadcastConv {
			if peer, err := d.ResolvePeer(conv); err == nil {
				conv = peer.PeerID
			}
		}
		if err := d.MarkRead(conv); err != nil {
			return fail(err)
		}
		return ok(true)
	case "verify":
		code, err := d.VerifyPeer(p.Peer)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"code": code})
	case "code":
		code, err := d.PeerCode(p.Peer)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"code": code, "me": d.Me()})
	default:
		return fail(fmt.Errorf("未知方法 %q", req.Method))
	}
}

var _ = store.BroadcastConv
