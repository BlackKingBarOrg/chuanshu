package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"chuanshu/internal/store"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func testModel() *model {
	return &model{
		mode: modePick,
		peers: []*store.PeerRecord{
			{PeerID: "id-wangwu", Nickname: "王五", Online: false},
			{PeerID: "id-zhangsan", Nickname: "张三", Online: true},
			{PeerID: "id-lisi", Nickname: "李四", Online: true},
		},
		input:  textinput.New(),
		unread: map[string]int{},
	}
}

func TestRosterOrderPeopleFirstBroadcastLast(t *testing.T) {
	m := testModel()
	convs := m.convs()
	if len(convs) != 4 {
		t.Fatalf("convs = %v", convs)
	}
	// Online first (by name), offline after, broadcast last so it
	// can never be the accidental default.
	if convs[3] != store.BroadcastConv {
		t.Fatalf("last conv = %q, want broadcast", convs[3])
	}
	if !m.peerOf(convs[0]).Online || !m.peerOf(convs[1]).Online {
		t.Fatalf("first two should be online peers: %v", convs)
	}
	if m.peerOf(convs[2]).Online {
		t.Fatalf("third should be the offline peer: %v", convs)
	}
	if m.selConv() == store.BroadcastConv {
		t.Fatal("default highlighted conversation must not be broadcast")
	}
}

func TestPickerSearchAndEnter(t *testing.T) {
	m := testModel()
	// Typing in the picker searches instead of composing a message.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("张")})
	m = updated.(*model)
	if m.filter != "张" {
		t.Fatalf("filter = %q, want 张", m.filter)
	}
	if got := m.selConv(); got != "id-zhangsan" {
		t.Fatalf("search selected %q, want id-zhangsan", got)
	}
	// Enter opens exactly that chat.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if m.mode != modeChat || m.conv != "id-zhangsan" {
		t.Fatalf("mode=%v conv=%q, want chat with id-zhangsan", m.mode, m.conv)
	}
	// Esc returns to the picker.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*model)
	if m.mode != modePick {
		t.Fatalf("after Esc mode = %v, want picker", m.mode)
	}
	if m.selConv() != "id-zhangsan" {
		t.Fatalf("picker highlight = %q, want back on id-zhangsan", m.selConv())
	}
}

// The composer must stay pinned: however long the history gets,
// the rendered frame keeps the same line count and the input sits
// on the same row.
func TestComposerPinnedAtBottom(t *testing.T) {
	m := testModel()
	m.mode = modeChat
	m.conv = "id-zhangsan"
	m.width, m.height = 100, 24
	m.input.Width = 60
	m.input.Placeholder = "输入消息，/file 加附件，Enter 发送"
	m.input.Focus()

	inputRow := func(view string) int {
		lines := strings.Split(view, "\n")
		if len(lines) != 24 {
			t.Fatalf("frame has %d lines, want exactly terminal height 24", len(lines))
		}
		for i, l := range lines {
			if strings.Contains(l, "入消息") { // placeholder, first rune styled apart
				return i
			}
		}
		t.Fatal("composer not found in frame")
		return -1
	}

	rowEmpty := inputRow(m.View())
	for i := 0; i < 200; i++ {
		m.messages = append(m.messages, &store.ChatMessage{
			ID: fmt.Sprint(i), Conv: m.conv, FromName: "张三",
			Text: fmt.Sprintf("第 %d 条消息", i), TS: time.Now(),
		})
	}
	rowLong := inputRow(m.View())
	if rowEmpty != rowLong {
		t.Fatalf("composer moved: row %d with empty history, row %d with 200 messages", rowEmpty, rowLong)
	}
	// Only the tail of the history is shown.
	if !strings.Contains(m.View(), "第 199 条消息") || strings.Contains(m.View(), "第 0 条消息") {
		t.Fatal("history should show the latest messages only")
	}
}

func TestAttachmentClickMapping(t *testing.T) {
	m := testModel()
	m.mode = modeChat
	m.conv = "id-zhangsan"
	m.width, m.height = 100, 24
	m.messages = []*store.ChatMessage{
		{ID: "1", Conv: m.conv, FromName: "张三", Text: "先看这个"},
		{ID: "2", Conv: m.conv, FromName: "张三", Attachment: &store.Attachment{Name: "cat.png", LocalPath: "/tmp/cat.png"}},
		{ID: "3", Conv: m.conv, FromName: "张三", Text: "看完回复我"},
	}
	if cm := m.messageAtRow(0); cm != nil {
		t.Fatal("header row must not map to a message")
	}
	if cm := m.messageAtRow(1); cm == nil || cm.Text != "先看这个" {
		t.Fatalf("row 1 = %+v, want first message", cm)
	}
	cm := m.messageAtRow(2)
	if cm == nil || cm.Attachment == nil || cm.Attachment.LocalPath != "/tmp/cat.png" {
		t.Fatalf("row 2 = %+v, want the attachment message", cm)
	}
	if cm := m.messageAtRow(99); cm != nil {
		t.Fatal("row beyond history must not map to a message")
	}
}

func TestMentionQueryAndFuzzyFilter(t *testing.T) {
	if q, ok := mentionQuery("看看这个 @rep"); !ok || q != "rep" {
		t.Fatalf("mentionQuery = %q,%v; want rep,true", q, ok)
	}
	if _, ok := mentionQuery("邮箱 a@b.com 里提到的"); ok {
		t.Fatal("@ inside a word must not open the file picker")
	}
	if _, ok := mentionQuery("没有提及"); ok {
		t.Fatal("no @ must not open the file picker")
	}
	index := []string{"/home/u/Downloads/report-2026.pdf", "/home/u/Documents/notes.txt", "/home/u/Pictures/cat.png"}
	got := filterFiles(index, "rep", 8)
	if len(got) == 0 || got[0] != index[0] {
		t.Fatalf("filterFiles(rep) should rank the basename match first, got %v", got)
	}
	got = filterFiles(index, "ctpng", 8) // subsequence fuzzy match
	if len(got) != 1 || got[0] != index[2] {
		t.Fatalf("filterFiles(ctpng) = %v", got)
	}
	if stripped := stripMention("发你 @rep"); stripped != "发你" {
		t.Fatalf("stripMention = %q, want 发你", stripped)
	}
}

func TestPickerArrowNavigationClamps(t *testing.T) {
	m := testModel()
	for i := 0; i < 10; i++ {
		m.moveSel(1)
	}
	if got := m.selConv(); got != store.BroadcastConv {
		t.Fatalf("after scrolling past end, sel = %q, want clamped at broadcast", got)
	}
	for i := 0; i < 10; i++ {
		m.moveSel(-1)
	}
	if m.sel != 0 {
		t.Fatalf("after scrolling past top, sel = %d, want 0", m.sel)
	}
}
