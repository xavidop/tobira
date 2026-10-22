package async

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartCancelsPrevious(t *testing.T) {
	var tr Tracker
	id1, ctx1 := tr.Start(context.Background())
	id2, ctx2 := tr.Start(context.Background())
	if id2 <= id1 {
		t.Fatalf("ids not increasing: %d then %d", id1, id2)
	}
	if ctx1.Err() == nil {
		t.Error("first context should be cancelled by the second Start")
	}
	if ctx2.Err() != nil {
		t.Error("second context should be live")
	}
}

func TestStartDerivesFromParent(t *testing.T) {
	var tr Tracker
	parent, cancel := context.WithCancel(context.Background())
	_, ctx := tr.Start(parent)
	cancel()
	if ctx.Err() == nil {
		t.Error("cancelling the parent should cancel the request")
	}
}

func TestStaleResultDropped(t *testing.T) {
	var tr Tracker
	id1, ctx1 := tr.Start(context.Background())
	id2, ctx2 := tr.Start(context.Background())

	msg1 := Do(id1, ctx1, func(context.Context) (int, error) { return 1, nil })().(Result[int])
	msg2 := Do(id2, ctx2, func(context.Context) (int, error) { return 2, nil })().(Result[int])
	if tr.Current(msg1.ID) {
		t.Error("first result should be stale")
	}
	if !tr.Current(msg2.ID) || msg2.Val != 2 {
		t.Errorf("second result should be current with Val 2, got %+v", msg2)
	}
}

func TestCurrentZeroValue(t *testing.T) {
	var tr Tracker
	if tr.Current(0) {
		t.Error("nothing started: no id is current")
	}
	id, _ := tr.Start(context.Background())
	tr.Cancel()
	if tr.Current(id) {
		t.Error("a cancelled request is no longer current")
	}
	tr.Cancel() // idempotent, no panic
}

func TestDoMeasuresDuration(t *testing.T) {
	var tr Tracker
	id, ctx := tr.Start(context.Background())
	res := Do(id, ctx, func(context.Context) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "x", nil
	})().(Result[string])
	if res.Took < 20*time.Millisecond || res.Took > 2*time.Second {
		t.Errorf("Took = %v, want about 20ms", res.Took)
	}
	if res.Val != "x" || res.Err != nil || res.ID != id {
		t.Errorf("unexpected result %+v", res)
	}
}

func TestDoHonoursCancel(t *testing.T) {
	var tr Tracker
	id, ctx := tr.Start(context.Background())
	cmd := Do(id, ctx, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	done := make(chan Result[int], 1)
	go func() { done <- cmd().(Result[int]) }()
	tr.Cancel()
	select {
	case res := <-done:
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("Err = %v, want context.Canceled", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command did not return after Cancel")
	}
}

func TestFinishReleasesTheContextOfTheLiveRequest(t *testing.T) {
	var tr Tracker
	id, ctx := tr.Start(context.Background())
	if ctx.Err() != nil {
		t.Fatal("context cancelled too early")
	}
	if !tr.Finish(id) {
		t.Fatal("the live request should finish")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Error("Finish should release the finished request's context")
	}
	if tr.Current(id) || tr.Finish(id) {
		t.Error("a finished request is no longer live")
	}
}

func TestFinishIgnoresStaleResults(t *testing.T) {
	var tr Tracker
	old, _ := tr.Start(context.Background())
	_, live := tr.Start(context.Background())
	if tr.Finish(old) {
		t.Error("a stale result must not finish")
	}
	if live.Err() != nil {
		t.Error("a stale Finish must not touch the live request")
	}
	if tr.Finish(0) {
		t.Error("id 0 is never live")
	}
}
