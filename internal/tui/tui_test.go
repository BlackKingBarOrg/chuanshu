package tui

import (
	"testing"

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
