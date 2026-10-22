package drivertest

import (
	"context"
	"runtime"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Close ends the Conn's watches before it returns: Watchers is 0 and the
// channel is closed at once, with no goroutine left to run first. A
// caller that checks Watchers right after Close (tobira watch's tests do)
// must never see the watch still open, however loaded the machine is.
func TestCloseEndsWatchesSynchronously(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for range 50 {
		f := NewFake(WithPeople())
		c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
		ch, err := c.(driver.Watcher).Watch(context.Background(), driver.WatchRequest{Collection: &ref})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if n := f.Watchers(); n != 0 {
			t.Fatalf("Watchers() = %d right after Close, want 0", n)
		}
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatal("got an event, want the channel closed")
			}
		default:
			t.Fatal("the watch channel is still open right after Close")
		}
	}
}

// Cancelling the watch's ctx still ends it, and a later Close does not
// close its channel twice.
func TestWatchCtxThenClose(t *testing.T) {
	f := NewFake(WithPeople())
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ref := driver.CollectionRef{Namespace: "main", Name: "tobira_people"}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.(driver.Watcher).Watch(ctx, driver.WatchRequest{Collection: &ref})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range ch {
	}
	_ = c.Close()
	if n := f.Watchers(); n != 0 {
		t.Fatalf("Watchers() = %d, want 0", n)
	}
}
