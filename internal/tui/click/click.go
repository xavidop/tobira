// Package click recognises double-clicks for the grid and the sidebar: a
// second click on the same target within a short window.
package click

import "time"

// Window is how soon the second click must follow the first.
const Window = 400 * time.Millisecond

// Tracker remembers the last click. The zero value has seen none. It is a
// value: a component keeps it next to its state and replaces it with what
// Hit returns.
type Tracker[T comparable] struct {
	target T
	set    bool
	at     time.Time
}

// Hit records a click on target (a comparable value: a cell, a node
// pointer) at time now. It reports whether this click completes a
// double-click: the previous one was on the same target no more than Window
// ago. A completed double-click is forgotten, so a third click starts over.
func (t Tracker[T]) Hit(target T, now time.Time) (Tracker[T], bool) {
	if t.set && t.target == target && now.Sub(t.at) <= Window {
		return Tracker[T]{}, true
	}
	return Tracker[T]{target: target, set: true, at: now}, false
}
