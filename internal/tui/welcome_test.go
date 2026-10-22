package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
	"github.com/xavidop/tobira/internal/session"
)

// The empty main panel lists the keys a newcomer needs, from the active
// keymap, instead of staying blank.
func TestWelcomeListsTheFirstKeys(t *testing.T) {
	h := newHarness(t)
	v := h.text()
	for _, want := range []string{
		"select a collection in the sidebar and press enter",
		"enter   open the selected connection or collection",
		"n       add a connection",
		"E       edit the selected connection",
		"/       filter the sidebar",
		"ctrl+e  open the query editor",
		":       command palette",
		"f1/?    every key and command",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("welcome missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "ask the AI") {
		t.Errorf("welcome advertises the AI without an assistant:\n%s", v)
	}
	// Opening a collection replaces it with the grid.
	h.keys("enter")
	h.until("main")
	h.keys("down", "enter")
	h.until("tobira_people")
	h.keys("down", "enter")
	h.until("Ada Lovelace")
	if strings.Contains(h.text(), "add a connection") {
		t.Errorf("welcome still shown with a collection open:\n%s", h.text())
	}
}

func TestWelcomeWithAnAssistant(t *testing.T) {
	h := newAIHarness(t, askFake(), nil)
	if v := h.text(); !strings.Contains(v, "a       ask the AI about the selected connection") {
		t.Errorf("welcome lacks the ask key with an assistant:\n%s", v)
	}
}

// With no connections at all the lead says to add one.
func TestWelcomeWithoutConnections(t *testing.T) {
	mgr := session.New(&config.Config{}, driver.NewRegistry(), secrets.Resolver{}, false)
	t.Cleanup(func() { _ = mgr.CloseAll() })
	model, err := New(Options{Manager: mgr, Theme: "tobira-dark"})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: model.(*Model), mgr: mgr, ch: make(chan tea.Msg, 4096)}
	h.run(h.m.Init())
	h.send(tea.WindowSizeMsg{Width: winW, Height: winH})
	if v := h.text(); !strings.Contains(v, "no connections yet: press n to add one") {
		t.Errorf("welcome lead with no connections:\n%s", v)
	}
}
