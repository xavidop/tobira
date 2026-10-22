package main

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/term"
)

// termOps are the terminal calls readSecret makes, so tests can stand in.
type termOps struct {
	getState func(fd int) (*term.State, error)
	restore  func(fd int, st *term.State) error
	read     func(fd int) ([]byte, error)
}

var realTermOps = termOps{getState: term.GetState, restore: term.Restore, read: term.ReadPassword}

// readSecret reads one line from the terminal on fd with no echo. When ctx
// is cancelled first (the signal handler swallows Ctrl-C into ctx, so the
// blocked read would otherwise carry on), it restores the terminal, writes a
// newline to w and returns the context's error. The terminal is restored on
// every exit.
func readSecret(ctx context.Context, fd int, w io.Writer, ops termOps) (string, error) {
	saved, err := ops.getState(fd)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := ops.read(fd)
		ch <- result{b, err}
	}()
	select {
	case <-ctx.Done():
		_ = ops.restore(fd, saved)
		_, _ = fmt.Fprintln(w)
		return "", ctx.Err()
	case r := <-ch:
		_ = ops.restore(fd, saved)
		if r.err != nil {
			clear(r.b)
			return "", fmt.Errorf("read password: %w", r.err)
		}
		s := string(r.b)
		clear(r.b)
		return s, nil
	}
}
