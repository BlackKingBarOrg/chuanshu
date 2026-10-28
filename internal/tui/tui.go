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

// Run starts the TUI program.
func Run(paths appcfg.Paths) error {
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
		status: "选择一位同事开始聊天",
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = p.Run()
	return err
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
	m.conv = conv
	m.mode = modeChat
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

// handleMouse: clicking a roster row selects it; clicking the
// selected row (or any row while chatting) opens that chat.
func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.MouseLeft {
		return m, nil
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
)

func (m *model) View() string {
	w, h := m.width, m.height
	if w == 0 {
		w, h = 100, 30
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
	footerLines := 3 // separator + hint + status (picker)
	if m.mode == modeChat {
		footerLines = 4 + suggestLines // separator + 发给 + input + status + popup
	}
	mainH := max(5, h-footerLines)

	right := m.viewRight(rightW, mainH)

	body := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftW).Height(mainH).MaxHeight(mainH).Render(left.String()),
		lipgloss.NewStyle().Width(rightW).Height(mainH).MaxHeight(mainH).Render(right),
	)
	status := m.status
	if m.offer != nil {
		status = warnStyle.Render(status)
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
			line := fmt.Sprintf("%s %s %s [%s %s]", ts, who, fileStyle.Render("文件"), a.Name, humanSize(a.Size))
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
