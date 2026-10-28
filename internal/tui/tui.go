// Package tui is the Bubble Tea front-end. It is a thin client of
// the daemon: all state lives in the daemon and on disk.
//
// Interaction model (like a normal chat app): you first pick who
// to talk to from the roster, then you chat. The app opens in the
// picker, never silently in the broadcast channel.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"chuanshu/internal/appcfg"
	"chuanshu/internal/ipc"
	"chuanshu/internal/store"
	"chuanshu/internal/wire"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type offerView struct {
	TransferID string         `json:"transfer_id"`
	FromID     string         `json:"from_id"`
	FromName   string         `json:"from_name"`
	File       *wire.FileMeta `json:"file"`
}

type uiMode int

const (
	modePick uiMode = iota // choosing who to talk to
	modeChat               // talking to model.conv
)

type model struct {
	paths      appcfg.Paths
	client     ipc.Client
	me         map[string]any
	peers      []*store.PeerRecord
	mode       uiMode
	sel        int    // index into visibleConvs()
	filter     string // roster search text (picker only)
	conv       string // active conversation in chat mode
	messages   []*store.ChatMessage
	unread     map[string]int
	input      textinput.Model
	status     string
	offer      *offerView
	attachList []string // pending attachments (chips above the input)
	progress   string
	// @-mention file completion state.
	fileIndex  []string
	suggest    []string
	suggestSel int
	completing bool
	hoverKey   string // attachment key currently under the mouse
	openPeer   string // conversation to open once peers load (chuanshu open)
	width      int
	height     int
	events     <-chan ipc.Event
}

type peersMsg struct{ peers []*store.PeerRecord }
type historyMsg struct {
	conv     string
	messages []*store.ChatMessage
	unread   map[string]int
}
type eventMsg struct{ ev ipc.Event }
type sentMsg struct {
	cm  *store.ChatMessage
	err error
}
type fileSentMsg struct{ err error }
type noticeMsg struct{ text string }
type attachMsg struct{ path string }

// Run starts the TUI program. openPeer, when set, jumps straight
// into that conversation once the roster loads.
func Run(paths appcfg.Paths, openPeer string) error {
	if err := ipc.EnsureDaemon(paths); err != nil {
		return err
	}
	client := ipc.Client{Paths: paths}
	events, err := client.Subscribe()
	if err != nil {
		return err
	}
	ti := textinput.New()
	ti.Placeholder = "输入消息，/file 加附件，Enter 发送"
	m := &model{
		paths: paths, client: client, mode: modePick,
		input: ti, events: events, unread: map[string]int{},
		status: "选择一位同事开始聊天", openPeer: openPeer,
	}
	// Register our Hyprland window with the daemon so that
	// `chuanshu open` (bar widget, notification) can focus us.
	if addr := detectWindowAddress(); addr != "" {
		var ok bool
		_ = client.Call("register_window", map[string]any{"address": addr}, &ok)
		defer func() {
			var ok bool
			_ = client.Call("unregister_window", map[string]any{"address": addr}, &ok)
		}()
	}
	// All-motion (not just cell-motion): plain hover must reach us
	// or attachment rows can never underline on hover.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion())
	_, err = p.Run()
	return err
}

// detectWindowAddress finds this terminal's Hyprland window by
// walking our ancestor pids (terminal is a grandparent) and
// matching them against `hyprctl clients`.
func detectWindowAddress() string {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return ""
	}
	ancestors := map[int]bool{}
	for pid := os.Getpid(); pid > 1; {
		ancestors[pid] = true
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		fields := strings.Fields(string(raw))
		if len(fields) < 4 {
			break
		}
		var ppid int
		if _, err := fmt.Sscan(fields[3], &ppid); err != nil || ppid <= 1 {
			break
		}
		pid = ppid
	}
	out, err := exec.Command("hyprctl", "clients", "-j").Output()
	if err != nil {
		return ""
	}
	var clients []struct {
		Address string `json:"address"`
		Pid     int    `json:"pid"`
	}
	if json.Unmarshal(out, &clients) != nil {
		return ""
	}
	for _, c := range clients {
		if ancestors[c.Pid] {
			return c.Address
		}
	}
	return ""
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.loadPeers(), m.waitEvent())
}

func (m *model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-m.events
		if !ok {
			return nil
		}
		return eventMsg{ev: ev}
	}
}

func (m *model) loadPeers() tea.Cmd {
	return func() tea.Msg {
		var peers []*store.PeerRecord
		if err := m.client.Call("peers", nil, &peers); err != nil {
			return noticeMsg{text: err.Error()}
		}
		var me map[string]any
		_ = m.client.Call("me", nil, &me)
		return peersMsg{peers: peers}
	}
}

// loadHistory loads a conversation. markRead is only true when the
// human actually opens the chat, never for picker previews.
func (m *model) loadHistory(conv string, markRead bool) tea.Cmd {
	if conv == "" {
		return nil
	}
	return func() tea.Msg {
		var out struct {
			Messages []*store.ChatMessage `json:"messages"`
			Unread   map[string]int       `json:"unread"`
		}
		if err := m.client.Call("history", map[string]any{"conv": conv}, &out); err != nil {
			return noticeMsg{text: err.Error()}
		}
		if markRead {
			_ = m.client.Call("mark_read", map[string]any{"conv": conv}, nil)
		}
		return historyMsg{conv: conv, messages: out.Messages, unread: out.Unread}
	}
}

// convs is the roster order: people first (online first, then by
// name), the all-hands channel last so it is never the accidental
// default.
func (m *model) convs() []string {
	sorted := append([]*store.PeerRecord{}, m.peers...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Online != sorted[j].Online {
			return sorted[i].Online
		}
		return sorted[i].Nickname < sorted[j].Nickname
	})
	out := make([]string, 0, len(sorted)+1)
	for _, p := range sorted {
		out = append(out, p.PeerID)
	}
	out = append(out, store.BroadcastConv)
	return out
}

// visibleConvs applies the picker search filter.
func (m *model) visibleConvs() []string {
	all := m.convs()
	if m.filter == "" {
		return all
	}
	f := strings.ToLower(m.filter)
	var out []string
	for _, c := range all {
		if strings.Contains(strings.ToLower(m.peerName(c)), f) || strings.Contains(strings.ToLower(c), f) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) selConv() string {
	vis := m.visibleConvs()
	if len(vis) == 0 {
		return ""
	}
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel >= len(vis) {
		m.sel = len(vis) - 1
	}
	return vis[m.sel]
}

// displayConv is the conversation whose history is on screen.
func (m *model) displayConv() string {
	if m.mode == modeChat {
		return m.conv
	}
	return m.selConv()
}

func (m *model) peerName(id string) string {
	if id == store.BroadcastConv {
		return "# 全员广播"
	}
	for _, p := range m.peers {
		if p.PeerID == id {
			return p.Nickname
		}
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m *model) peerOf(id string) *store.PeerRecord {
	for _, p := range m.peers {
		if p.PeerID == id {
			return p
		}
	}
	return nil
}

// moveSel moves the picker highlight and previews that person.
func (m *model) moveSel(delta int) tea.Cmd {
	vis := m.visibleConvs()
	if len(vis) == 0 {
		return nil
	}
	m.sel += delta
	if m.sel < 0 {
		m.sel = 0
	}
	if m.sel >= len(vis) {
		m.sel = len(vis) - 1
	}
	m.messages = nil
	return m.loadHistory(m.selConv(), false)
}

// enterChat opens the highlighted conversation for chatting.
func (m *model) enterChat() tea.Cmd {
	conv := m.selConv()
	if conv == "" {
		return nil
	}
	return m.enterConv(conv)
}

// enterConv opens a specific conversation for chatting.
func (m *model) enterConv(conv string) tea.Cmd {
	if conv == "" {
		return nil
	}
	m.conv = conv
	m.mode = modeChat
	m.filter = ""
	// Keep the picker highlight in sync for when we go back.
	for i, c := range m.convs() {
		if c == conv {
			m.sel = i
			break
		}
	}
	m.input.Focus()
	m.messages = nil
	if conv == store.BroadcastConv {
		m.status = "群聊：将发送给所有在线同事"
	} else {
		m.status = "正在与 " + m.peerName(conv) + " 聊天"
	}
	return m.loadHistory(conv, true)
}

// backToPick returns to the roster, keeping the highlight on the
// conversation just left.
func (m *model) backToPick() tea.Cmd {
	m.mode = modePick
	m.input.Blur()
	m.filter = ""
	vis := m.visibleConvs()
	for i, c := range vis {
		if c == m.conv {
			m.sel = i
			break
		}
	}
	m.status = "选择一位同事开始聊天"
	return m.loadHistory(m.selConv(), false)
}

// quickSwitch jumps between conversations without leaving chat
// mode (Ctrl+N / Ctrl+P).
func (m *model) quickSwitch(delta int) tea.Cmd {
	vis := m.convs()
	if len(vis) == 0 {
		return nil
	}
	idx := 0
	for i, c := range vis {
		if c == m.conv {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(vis)) % len(vis)
	m.sel = idx
	m.filter = ""
	return m.enterChat()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(20, msg.Width-30)
		return m, nil
	case peersMsg:
		m.peers = msg.peers
		if m.openPeer != "" {
			target := m.openPeer
			m.openPeer = ""
			for _, c := range m.convs() {
				if c == target || m.peerName(c) == target {
					return m, m.enterConv(c)
				}
			}
		}
		if m.mode == modePick {
			return m, m.loadHistory(m.selConv(), false)
		}
		return m, nil
	case historyMsg:
		if msg.conv == m.displayConv() {
			m.messages = msg.messages
		}
		if msg.unread != nil {
			m.unread = msg.unread
		}
		return m, nil
	case noticeMsg:
		m.status = msg.text
		return m, nil
	case attachMsg:
		m.addAttach(msg.path)
		m.status = "已添加附件: " + filepath.Base(msg.path) + "（Enter 发送，Ctrl+X 清空）"
		return m, nil
	case sentMsg:
		if msg.err != nil {
			m.status = "发送失败: " + msg.err.Error()
		} else {
			m.status = "已发送给 " + m.peerName(m.conv)
		}
		return m, m.loadHistory(m.conv, true)
	case fileSentMsg:
		if msg.err != nil {
			m.status = "文件发送失败: " + msg.err.Error()
		} else {
			m.status = "文件已送达 " + m.peerName(m.conv)
			m.attachList = nil
		}
		return m, m.loadHistory(m.conv, true)
	case eventMsg:
		return m, tea.Batch(m.handleEvent(msg.ev), m.waitEvent())
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	if m.mode == modeChat {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) handleEvent(ev ipc.Event) tea.Cmd {
	switch ev.Kind {
	case "peers":
		var peers []*store.PeerRecord
		if json.Unmarshal(ev.Data, &peers) == nil {
			m.peers = peers
		}
	case "message":
		var cm store.ChatMessage
		if json.Unmarshal(ev.Data, &cm) == nil {
			return m.loadHistory(m.displayConv(), m.mode == modeChat)
		}
	case "focus":
		var f struct {
			Conv string `json:"conv"`
		}
		if json.Unmarshal(ev.Data, &f) == nil && f.Conv != "" {
			return m.enterConv(f.Conv)
		}
	case "offer":
		var o offerView
		if json.Unmarshal(ev.Data, &o) == nil {
			m.offer = &o
			m.status = fmt.Sprintf("%s 发来文件 %s（%s），按 y 接收 / n 拒绝", o.FromName, o.File.Name, humanSize(o.File.Size))
		}
	case "transfer":
		var t struct {
			TransferID string `json:"transfer_id"`
			Sent       int64  `json:"sent"`
			Size       int64  `json:"size"`
			Outgoing   bool   `json:"outgoing"`
		}
		if json.Unmarshal(ev.Data, &t) == nil && t.Size > 0 {
			dir := "接收"
			if t.Outgoing {
				dir = "发送"
			}
			m.progress = fmt.Sprintf("%s中 %d%%", dir, t.Sent*100/t.Size)
			m.status = m.progress
		}
	case "notice":
		var n struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &n) == nil {
			m.status = n.Text
		}
	}
	return nil
}

// visibleMessages is the tail of history actually rendered.
func (m *model) visibleMessages() []*store.ChatMessage {
	limit := max(1, m.mainHeight()-1)
	if m.mode == modePick {
		limit = max(1, m.mainHeight()-7)
	}
	if len(m.messages) <= limit {
		return m.messages
	}
	return m.messages[len(m.messages)-limit:]
}

// messageAtRow maps a terminal row in the right pane to a
// message, in both chat mode (messages start under the header)
// and picker mode (preview under the peer card).
func (m *model) messageAtRow(y int) *store.ChatMessage {
	offset := 1 // chat: row 0 is the chat header
	if m.mode == modePick {
		offset = 6 // card: name, state, blank, Enter, blank, 最近消息:
	}
	vis := m.visibleMessages()
	idx := y - offset
	if idx < 0 || idx >= len(vis) {
		return nil
	}
	return vis[idx]
}

// openAttachment opens a file with the system default app, or
// reveals it in the file manager when reveal is true.
func openAttachment(path string, reveal bool) tea.Cmd {
	return func() tea.Msg {
		if reveal {
			// Prefer a file manager that selects the file; fall
			// back to opening its folder.
			if nautilus, err := exec.LookPath("nautilus"); err == nil {
				if err := exec.Command(nautilus, "--select", path).Start(); err == nil {
					return noticeMsg{text: "已在文件管理器定位: " + filepath.Base(path)}
				}
			}
			if err := exec.Command("xdg-open", filepath.Dir(path)).Start(); err != nil {
				return noticeMsg{text: "打开文件管理器失败: " + err.Error()}
			}
			return noticeMsg{text: "已在文件管理器打开所在文件夹: " + filepath.Base(path)}
		}
		if err := exec.Command("xdg-open", path).Start(); err != nil {
			return noticeMsg{text: "打开失败: " + err.Error()}
		}
		return noticeMsg{text: "已用系统程序打开: " + filepath.Base(path)}
	}
}

// clickReveal decides whether a mouse click on an attachment
// reveals it in the file manager (Alt+click or right click)
// instead of opening it (plain click and Ctrl+click both open).
func clickReveal(msg tea.MouseMsg) bool {
	return msg.Type == tea.MouseRight || msg.Alt
}

// handleMouse: clicking a roster row selects it; clicking the
// selected row (or any row while chatting) opens that chat.
// Clicking an attachment row in the chat opens it; Ctrl+click
// reveals it in the file manager.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Hovering an attachment row underlines it (see View), so it
	// reads as clickable before the user commits to a click.
	if msg.Action == tea.MouseActionMotion || msg.Type == tea.MouseMotion {
		m.hoverKey = ""
		if msg.X >= 26 && msg.Y >= 1 && msg.Y < m.mainHeight() {
			if cm := m.messageAtRow(msg.Y); cm != nil && cm.Attachment != nil && cm.Attachment.LocalPath != "" {
				m.hoverKey = cm.Attachment.TransferID
				if m.hoverKey == "" {
					m.hoverKey = cm.Attachment.LocalPath
				}
			}
		}
		return m, nil
	}
	if msg.Type != tea.MouseLeft && msg.Type != tea.MouseRight {
		return m, nil
	}
	if msg.X >= 26 && msg.Y >= 1 && msg.Y < m.mainHeight() {
		if cm := m.messageAtRow(msg.Y); cm != nil && cm.Attachment != nil && cm.Attachment.LocalPath != "" {
			return m, openAttachment(cm.Attachment.LocalPath, clickReveal(msg))
		}
		if m.mode == modePick {
			// Clicking elsewhere in the preview enters the chat.
			return m, m.enterChat()
		}
		return m, nil
	}
	if msg.Type == tea.MouseRight {
		return m, nil // right click only means "reveal attachment"
	}
	row := msg.Y - 1 // row 0 is the roster title
	vis := m.visibleConvs()
	if msg.X > 26 || row < 0 || row >= len(vis) {
		return m, nil
	}
	if m.mode == modeChat {
		m.sel = row
		return m, m.enterChat()
	}
	if row == m.sel {
		return m, m.enterChat()
	}
	m.sel = row
	m.messages = nil
	return m, m.loadHistory(m.selConv(), false)
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending file offer captures y/n, but never steals keys
	// while the user is mid-sentence.
	if m.offer != nil && (m.mode == modePick || m.input.Value() == "") {
		switch msg.String() {
		case "y", "Y":
			offer := m.offer
			m.offer = nil
			m.status = "已确认接收，等待传输…"
			return m, func() tea.Msg {
				var ok bool
				err := m.client.Call("respond_offer", map[string]any{"transfer_id": offer.TransferID, "accept": true}, &ok)
				if err != nil {
					return noticeMsg{text: err.Error()}
				}
				return noticeMsg{text: "开始接收 " + offer.File.Name}
			}
		case "n", "N":
			offer := m.offer
			m.offer = nil
			m.status = "已拒绝"
			return m, func() tea.Msg {
				var ok bool
				_ = m.client.Call("respond_offer", map[string]any{"transfer_id": offer.TransferID, "accept": false}, &ok)
				return noticeMsg{text: "已拒绝接收"}
			}
		case "ctrl+c":
			return m, tea.Quit
		}
	}
	if m.mode == modePick {
		return m.handlePickKey(msg)
	}
	return m.handleChatKey(msg)
}

// handlePickKey: arrows/jk move, typing searches, Enter chats.
func (m *model) handlePickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyUp:
		return m, m.moveSel(-1)
	case tea.KeyDown:
		return m, m.moveSel(1)
	case tea.KeyEnter:
		return m, m.enterChat()
	case tea.KeyBackspace:
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.sel = 0
			return m, m.loadHistory(m.selConv(), false)
		}
		return m, nil
	case tea.KeyEsc:
		m.filter = ""
		m.sel = 0
		return m, m.loadHistory(m.selConv(), false)
	case tea.KeyRunes:
		switch string(msg.Runes) {
		case "k":
			return m, m.moveSel(-1)
		case "j":
			return m, m.moveSel(1)
		}
		m.filter += msg.String()
		m.sel = 0
		m.messages = nil
		return m, m.loadHistory(m.selConv(), false)
	}
	if msg.Paste {
		m.filter += strings.TrimSpace(msg.String())
		m.sel = 0
		return m, m.loadHistory(m.selConv(), false)
	}
	return m, nil
}

func (m *model) handleChatKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste {
		content := strings.TrimSpace(msg.String())
		if p, ok := existingPath(content); ok {
			m.addAttach(p)
			m.status = "已添加附件: " + filepath.Base(p) + "（Enter 发送）"
			return m, nil
		}
	}
	// While the @ file picker is open, navigation keys belong to it.
	if m.completing {
		switch msg.Type {
		case tea.KeyDown, tea.KeyCtrlN:
			if len(m.suggest) > 0 {
				m.suggestSel = (m.suggestSel + 1) % len(m.suggest)
			}
			return m, nil
		case tea.KeyUp, tea.KeyCtrlP:
			if len(m.suggest) > 0 {
				m.suggestSel = (m.suggestSel - 1 + len(m.suggest)) % len(m.suggest)
			}
			return m, nil
		case tea.KeyTab, tea.KeyEnter:
			m.acceptSuggest()
			return m, nil
		case tea.KeyEsc:
			m.completing = false
			m.suggest = nil
			return m, nil
		}
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEsc:
		return m, m.backToPick()
	case tea.KeyCtrlN:
		return m, m.quickSwitch(1)
	case tea.KeyCtrlP:
		return m, m.quickSwitch(-1)
	case tea.KeyCtrlV:
		return m, m.pasteFromClipboard()
	case tea.KeyCtrlO:
		return m, m.openLastAttachment()
	case tea.KeyCtrlX:
		m.attachList = nil
		m.status = "已清空待发附件"
		return m, nil
	case tea.KeyTab:
		if strings.HasPrefix(m.input.Value(), "/file ") {
			completed := completePath(strings.TrimPrefix(m.input.Value(), "/file "))
			if completed != "" {
				m.input.SetValue("/file " + completed)
				m.input.CursorEnd()
			}
			return m, nil
		}
	case tea.KeyEnter:
		return m, m.submit()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshSuggest()
	return m, cmd
}

func (m *model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	attachments := append([]string{}, m.attachList...)
	m.input.SetValue("")
	m.attachList = nil
	m.completing = false
	m.suggest = nil
	if strings.HasPrefix(text, "/file ") {
		p := strings.Trim(strings.TrimPrefix(text, "/file "), "\"'")
		if p != "" {
			attachments = append(attachments, p)
		}
		text = ""
	}
	if strings.HasPrefix(text, "/quit") {
		return tea.Quit
	}
	if len(attachments) > 0 && m.conv == store.BroadcastConv {
		m.status = "广播频道不能发文件，请 Esc 回列表选一位同事"
		m.attachList = attachments
		return nil
	}
	if len(attachments) == 0 && text == "" {
		return nil
	}
	conv := m.conv
	m.status = "正在发送…"
	return func() tea.Msg {
		for _, p := range attachments {
			if _, err := m.client.RawCall("send_file", map[string]any{"peer": conv, "path": p}); err != nil {
				return fileSentMsg{err: err}
			}
		}
		if text == "" {
			return fileSentMsg{}
		}
		raw, err := m.client.RawCall("send_text", map[string]any{"peer": conv, "text": text})
		var cm store.ChatMessage
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &cm)
		}
		if err != nil {
			return sentMsg{cm: &cm, err: err}
		}
		return sentMsg{cm: &cm}
	}
}

// pasteFromClipboard mirrors coding-agent terminals: an image on
// the clipboard becomes an attachment; file paths attach too.
func (m *model) pasteFromClipboard() tea.Cmd {
	return func() tea.Msg {
		typesOut, err := exec.Command("wl-paste", "--list-types").Output()
		if err == nil {
			types := string(typesOut)
			for _, mime := range []string{"image/png", "image/jpeg", "image/webp"} {
				if strings.Contains(types, mime) {
					data, err := exec.Command("wl-paste", "--type", mime).Output()
					if err == nil && len(data) > 0 {
						ext := ".png"
						if mime == "image/jpeg" {
							ext = ".jpg"
						}
						name := filepath.Join(m.paths.CacheDir, "paste-"+time.Now().Format("20060102-150405")+ext)
						if err := os.WriteFile(name, data, 0o644); err == nil {
							return attachMsg{path: name}
						}
					}
				}
			}
		}
		out, err := exec.Command("wl-paste", "--no-newline").Output()
		if err == nil {
			if p, ok := existingPath(strings.TrimSpace(string(out))); ok {
				return attachMsg{path: p}
			}
		}
		return noticeMsg{text: "剪贴板里没有图片或文件"}
	}
}

func (m *model) openLastAttachment() tea.Cmd {
	for i := len(m.messages) - 1; i >= 0; i-- {
		a := m.messages[i].Attachment
		if a != nil && a.LocalPath != "" {
			path := a.LocalPath
			return func() tea.Msg {
				cmd := exec.Command("xdg-open", path)
				if err := cmd.Start(); err != nil {
					return noticeMsg{text: "打开失败: " + err.Error()}
				}
				return noticeMsg{text: "已用系统程序打开: " + filepath.Base(path)}
			}
		}
	}
	m.status = "本会话没有可打开的附件"
	return nil
}

func existingPath(s string) (string, bool) {
	s = strings.Trim(s, "\"'")
	if s == "" || strings.Contains(s, "\n") {
		return "", false
	}
	if _, err := os.Stat(s); err == nil {
		return s, true
	}
	return "", false
}

func completePath(prefix string) string {
	prefix = strings.Trim(prefix, "\"'")
	dir := filepath.Dir(prefix)
	base := filepath.Base(prefix)
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		dir = prefix
		base = ""
	}
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base) {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				p += "/"
			}
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
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

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	meStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	fileStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	selectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("15")).Bold(true)
	onlineStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	accentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	hoverStyle    = lipgloss.NewStyle().Underline(true).Foreground(lipgloss.Color("6")).Bold(true).Background(lipgloss.Color("238"))
	linkStyle     = lipgloss.NewStyle().Underline(true).Foreground(lipgloss.Color("6"))
)

func (m *model) View() string {
	w := m.width
	if w == 0 {
		w = 100
	}
	leftW := 26
	rightW := max(30, w-leftW-3)

	// --- roster ---
	var left strings.Builder
	title := "聊天对象"
	if m.mode == modePick {
		title = accentStyle.Render("聊天对象（选人）")
	} else {
		title = titleStyle.Render(title)
	}
	left.WriteString(title + "\n")
	vis := m.visibleConvs()
	for i, conv := range vis {
		name := m.displayName(conv)
		if n := m.unread[conv]; n > 0 {
			name += fmt.Sprintf(" (%d)", n)
		}
		line := "  " + name
		if m.mode == modePick && i == m.sel {
			line = selectedStyle.Render("▸ " + name)
		} else if m.mode == modeChat && conv == m.conv {
			line = accentStyle.Render("▸ " + name)
		}
		left.WriteString(line + "\n")
	}
	if len(vis) == 0 {
		left.WriteString(dimStyle.Render("  没有匹配的同事") + "\n")
	}
	if m.filter != "" {
		left.WriteString("\n" + accentStyle.Render("搜索: "+m.filter) + "\n")
	}
	if m.mode == modePick {
		left.WriteString("\n" + dimStyle.Render("↑↓/jk 选择 · Enter 聊天\n打字搜索 · 单击选/双击进"))
	} else {
		left.WriteString("\n" + dimStyle.Render("Esc 回列表选人\nCtrl+N/P 快速切换"))
	}

	// Fixed layout (coding-agent style): the history pane gets
	// exactly the rows left over above a pinned composer. However
	// long the history grows, the input never moves.
	suggestLines := 0
	if m.mode == modeChat && m.completing && len(m.suggest) > 0 {
		suggestLines = len(m.suggest)
	}
	mainH := m.mainHeight()

	right := m.viewRight(rightW, mainH)

	body := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Height(mainH).MaxHeight(mainH).Render(left.String()),
		lipgloss.NewStyle().Width(rightW).Height(mainH).MaxHeight(mainH).Render(right),
	)
	status := m.status
	if m.offer != nil {
		status = warnStyle.Render(status)
	}
	if m.hoverKey != "" {
		status += dimStyle.Render(" · 点击打开，Alt+点击/右键在文件管理器显示")
	}
	var footer strings.Builder
	if suggestLines > 0 {
		for i, s := range m.suggest {
			label := "  " + shortenHome(s)
			if i == m.suggestSel {
				label = selectedStyle.Render("▸ " + shortenHome(s))
			}
			footer.WriteString(label + "\n")
		}
	}
	footer.WriteString(strings.Repeat("─", min(w, leftW+rightW+3)) + "\n")
	if m.mode == modeChat {
		toLine := "发给 " + m.peerName(m.conv) + ": 输入 @ 选文件 · Ctrl+V 粘贴图片"
		if len(m.attachList) > 0 {
			names := make([]string, 0, len(m.attachList))
			for _, a := range m.attachList {
				names = append(names, filepath.Base(a))
			}
			toLine = "发给 " + m.peerName(m.conv) + " · 附件: " + strings.Join(names, ", ") + "（Enter 发送，Ctrl+X 清空）"
		}
		footer.WriteString(dimStyle.Render(toLine) + "\n")
		footer.WriteString(m.input.View() + "\n")
	} else {
		footer.WriteString(dimStyle.Render("在左侧选一位同事，按 Enter 开始聊天") + "\n")
	}
	return body + "\n" + footer.String() + dimStyle.Render(status)
}

// rightWidth is the chat pane width for the current frame.
func (m *model) rightWidth() int {
	w := m.width
	if w == 0 {
		w = 100
	}
	return max(30, w-26-3)
}

// truncRunes shortens s to at most n runes with an ellipsis.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// mainHeight is the fixed history-pane height for this frame.
func (m *model) mainHeight() int {
	h := m.height
	if h == 0 {
		h = 30
	}
	suggestLines := 0
	if m.mode == modeChat && m.completing && len(m.suggest) > 0 {
		suggestLines = len(m.suggest)
	}
	footerLines := 3
	if m.mode == modeChat {
		footerLines = 4 + suggestLines
	}
	return max(5, h-footerLines)
}

// shortenHome renders ~ for the home directory in picker rows.
func shortenHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// displayName renders one roster entry with presence.
func (m *model) displayName(conv string) string {
	if conv == store.BroadcastConv {
		return "# 全员广播（群聊）"
	}
	p := m.peerOf(conv)
	if p == nil {
		return m.peerName(conv)
	}
	dot := "○"
	if p.Online {
		dot = onlineStyle.Render("●")
	}
	name := fmt.Sprintf("%s %s", dot, p.Nickname)
	if p.Verified {
		name += " ✓"
	}
	return name
}

func (m *model) viewRight(rightW, h int) string {
	if m.mode == modePick {
		return m.viewPickCard(h)
	}
	return m.viewChat(h)
}

// viewPickCard shows who is highlighted and what Enter will do.
func (m *model) viewPickCard(h int) string {
	conv := m.selConv()
	var b strings.Builder
	if conv == "" || (len(m.peers) == 0 && conv == store.BroadcastConv) {
		b.WriteString(titleStyle.Render("还没有发现同事") + "\n\n")
		b.WriteString("确认同事已启动 chuanshu 且在同一局域网。\n广播被屏蔽时可用命令行手动连接:\n  chuanshu peer-add <对方IP>:24247")
		return b.String()
	}
	if conv == store.BroadcastConv {
		b.WriteString(titleStyle.Render("# 全员广播") + "\n")
		b.WriteString(warnStyle.Render("群聊频道 · 消息将发给所有在线同事") + "\n\n")
	} else {
		p := m.peerOf(conv)
		b.WriteString(titleStyle.Render(m.peerName(conv)) + "\n")
		state := "离线"
		if p != nil && p.Online {
			state = onlineStyle.Render("在线")
		}
		verified := "未核对身份（chuanshu code 核对）"
		if p != nil && p.Verified {
			verified = "身份已核对 ✓"
		}
		b.WriteString(fmt.Sprintf("%s · %s\n\n", state, verified))
	}
	b.WriteString(accentStyle.Render("按 Enter 开始聊天") + "\n")
	if len(m.messages) > 0 {
		b.WriteString("\n" + dimStyle.Render("最近消息:") + "\n")
		b.WriteString(m.messageLines(max(1, h-7)))
	}
	return b.String()
}

func (m *model) viewChat(h int) string {
	var b strings.Builder
	if m.conv == store.BroadcastConv {
		b.WriteString(titleStyle.Render("# 全员广播") + "  " + warnStyle.Render("群聊 · 所有在线同事都会收到") + "\n")
	} else {
		state := "离线（发送将失败）"
		if p := m.peerOf(m.conv); p != nil && p.Online {
			state = onlineStyle.Render("在线") + " · 已加密"
		}
		b.WriteString(titleStyle.Render("与 "+m.peerName(m.conv)+" 聊天") + "  " + dimStyle.Render(state) + "\n")
	}
	b.WriteString(m.messageLines(max(1, h-1)))
	return b.String()
}

func (m *model) messageLines(limit int) string {
	lines := []string{}
	for _, cm := range m.messages {
		who := cm.FromName
		if cm.Mine {
			who = meStyle.Render("我")
		}
		ts := cm.TS.Format("15:04")
		if cm.Attachment != nil {
			a := cm.Attachment
			// Keep every message on exactly one terminal row:
			// click-to-row mapping depends on it.
			name := truncRunes(a.Name, 24)
			key := a.TransferID
			if key == "" {
				key = a.LocalPath
			}
			// Attachments always render as links (underlined),
			// so clickability never depends on the terminal
			// bothering to report mouse motion; hover intensifies.
			if m.hoverKey != "" && m.hoverKey == key {
				name = hoverStyle.Render(name)
			} else {
				name = linkStyle.Render(name)
			}
			line := fmt.Sprintf("%s %s %s [%s %s]", ts, who, fileStyle.Render("文件"), name, humanSize(a.Size))
			if a.LocalPath != "" {
				suffix := " ↗点击打开·Alt+定位"
				if lipgloss.Width(line)+lipgloss.Width(suffix) <= m.rightWidth() {
					line += dimStyle.Render(suffix)
				}
			}
			if cm.Status == "failed" {
				line += errStyle.Render(" 发送失败")
			}
			lines = append(lines, line)
			continue
		}
		line := fmt.Sprintf("%s %s  %s", ts, who, cm.Text)
		switch cm.Status {
		case "failed":
			line += errStyle.Render(" ✗ 对方不在线")
		case "delivered":
			line += dimStyle.Render(" ✓")
		case "read":
			line += dimStyle.Render(" ✓✓")
		}
		lines = append(lines, line)
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
