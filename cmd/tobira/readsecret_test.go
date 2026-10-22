package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/term"
)

func TestReadSecretReturnsTheLineAndZeroesTheBuffer(t *testing.T) {
	buf := []byte("hunter2")
	ops := termOps{
		getState: func(int) (*term.State, error) { return &term.State{}, nil },
		restore:  func(int, *term.State) error { return nil },
		read:     func(int) ([]byte, error) { return buf, nil },
	}
	got, err := readSecret(context.Background(), 0, &bytes.Buffer{}, ops)
	if err != nil || got != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !bytes.Equal(buf, make([]byte, 7)) {
		t.Errorf("buffer not zeroed: %q", buf)
	}
}

func TestReadSecretRestoresTheTerminalOnCancel(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	restored := make(chan struct{}, 1)
	ops := termOps{
		getState: func(int) (*term.State, error) { return &term.State{}, nil },
		restore:  func(int, *term.State) error { restored <- struct{}{}; return nil },
		read:     func(int) ([]byte, error) { <-block; return nil, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := readSecret(ctx, 0, &out, ops); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readSecret did not return on cancel")
	}
	select {
	case <-restored:
	default:
		t.Error("terminal not restored")
	}
	if out.String() != "\n" {
		t.Errorf("output = %q, want a newline", out.String())
	}
}

func TestReadSecretRestoresOnReadError(t *testing.T) {
	restored := 0
	ops := termOps{
		getState: func(int) (*term.State, error) { return &term.State{}, nil },
		restore:  func(int, *term.State) error { restored++; return nil },
		read:     func(int) ([]byte, error) { return nil, errors.New("boom") },
	}
	if _, err := readSecret(context.Background(), 0, &bytes.Buffer{}, ops); err == nil {
		t.Fatal("no error")
	}
	if restored != 1 {
		t.Errorf("restored %d times", restored)
	}
}
