package mcpserver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// sessionIdle is how long an HTTP session may go without a request before
// it is dropped, with its client name: clients that never send DELETE do
// not hold memory forever.
const sessionIdle = 30 * time.Minute

// sessionTable issues and validates the HTTP session IDs (an mcp-go
// SessionIdManager). Only IDs it issued and that are live are accepted; an
// ended, expired or unknown one is answered 404, which tells the client to
// initialize again. A missing one is 400.
type sessionTable struct {
	now    func() time.Time
	idle   time.Duration
	expire func(id string) // drops what the server keeps for a session

	mu   sync.Mutex
	live map[string]time.Time // ID -> last request
}

func newSessionTable(now func() time.Time, expire func(string)) *sessionTable {
	return &sessionTable{now: now, idle: sessionIdle, expire: expire, live: map[string]time.Time{}}
}

var errNoSession = errors.New("no session ID")

// Generate issues a new session ID.
func (t *sessionTable) Generate() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	id := "mcp-session-" + hex.EncodeToString(b)
	t.sweep()
	t.mu.Lock()
	t.live[id] = t.now()
	t.mu.Unlock()
	return id
}

// Validate accepts a live ID and marks it used; anything else is ended.
func (t *sessionTable) Validate(id string) (isTerminated bool, err error) {
	if id == "" {
		return false, errNoSession
	}
	t.sweep() // expires id too when it has been idle too long
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.live[id]; !ok {
		return true, nil
	}
	t.live[id] = t.now()
	return false, nil
}

// Terminate ends id (DELETE). Ending an unknown ID is not an error.
func (t *sessionTable) Terminate(id string) (isNotAllowed bool, err error) {
	t.mu.Lock()
	delete(t.live, id)
	t.mu.Unlock()
	return false, nil
}

// sweep drops every session idle for longer than the limit.
func (t *sessionTable) sweep() {
	now := t.now()
	var gone []string
	t.mu.Lock()
	for id, last := range t.live {
		if now.Sub(last) > t.idle {
			delete(t.live, id)
			gone = append(gone, id)
		}
	}
	t.mu.Unlock()
	for _, id := range gone {
		t.expire(id)
	}
}
