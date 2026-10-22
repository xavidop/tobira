package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Renaming the editor's connection through the edit form carries its
// buffer, its last-run text and the editor itself to the new name: the
// query stays visible and runs on the renamed connection, and quitting does
// not ask about a buffer that was run as it is.
func TestRenameCarriesTheEditorBuffer(t *testing.T) {
	h, _ := newConnHarness(t)
	const text = "select * from tobira_people"
	h.send(ctrl('e'))
	h.send(tea.PasteMsg{Content: text})
	h.runUntilDone(keyF5)
	h.keys("esc")
	h.m.active = 0
	h.m.focus = focusSidebar
	h.step("E")
	h.typ("2")
	h.step("ctrl+s")
	h.until("saved local2")
	h.settle()
	if h.m.edConn != "local2" {
		t.Fatalf("editor connection = %q, want local2", h.m.edConn)
	}
	if got := h.m.ed.Value(); got != text {
		t.Fatalf("editor buffer after the rename = %q, want %q", got, text)
	}
	if _, stranded := h.m.buffers["local"]; stranded {
		t.Fatal("the buffer was left under the old name")
	}
	if _, stranded := h.m.runBuf["local"]; stranded {
		t.Fatal("the last-run text was left under the old name")
	}
	h.send(ctrl('e'))
	h.runUntilDone(keyF5)
	last := h.m.tabs[len(h.m.tabs)-1]
	if last.conn != "local2" || last.state != tabDone {
		t.Fatalf("run after the rename: conn %q state %v", last.conn, last.state)
	}
	h.keys("esc")
	h.keys("q")
	if h.m.ask != nil {
		t.Fatalf("quitting asked %q after the rename", h.m.ask.title)
	}
	if !h.quitted() {
		t.Fatal("q did not quit")
	}
}

// A rename through the edit form keeps a :readonly on override; a
// removal drops it.
func TestReadOnlyOverrideFollowsARename(t *testing.T) {
	h, _ := newConnHarness(t)
	if err := h.mgr.SetReadOnly("local", true); err != nil {
		t.Fatal(err)
	}
	h.step("E")
	h.typ("2")
	h.step("ctrl+s")
	h.until("saved local2")
	if !h.mgr.State("local2").ReadOnly {
		t.Fatal("the rename dropped the :readonly override")
	}
	h.m.sb = h.m.sb.Select("local2")
	h.m.focus = focusSidebar
	h.step("D")
	h.typ("local2")
	h.step("enter")
	h.until("removed local2")
	// Re-add the name: the removed entry's override is gone with it.
	if err := h.mgr.SetReadOnly("local2", false); err == nil {
		t.Fatal("the removed connection still exists")
	}
}
