package click

import (
	"testing"
	"time"
)

func TestWindowIs400ms(t *testing.T) {
	if Window != 400*time.Millisecond {
		t.Fatalf("Window = %v, want 400ms", Window)
	}
}

func TestHit(t *testing.T) {
	t0 := time.Unix(100, 0)
	type cell struct{ r, c int }

	tr, dbl := Tracker[cell]{}.Hit(cell{1, 2}, t0)
	if dbl {
		t.Fatal("the first click is never a double-click")
	}
	if _, dbl = tr.Hit(cell{1, 2}, t0.Add(Window)); !dbl {
		t.Error("same target at exactly the window is a double-click")
	}
	if _, dbl = tr.Hit(cell{1, 2}, t0.Add(Window+time.Millisecond)); dbl {
		t.Error("a click past the window is not a double-click")
	}
	if _, dbl = tr.Hit(cell{1, 3}, t0.Add(time.Millisecond)); dbl {
		t.Error("a different target is not a double-click")
	}
	after, _ := tr.Hit(cell{1, 2}, t0.Add(time.Millisecond))
	if _, dbl = after.Hit(cell{1, 2}, t0.Add(2*time.Millisecond)); dbl {
		t.Error("a third click starts a new pair")
	}
	var empty Tracker[*int]
	if _, dbl = empty.Hit(nil, t0); dbl {
		t.Error("the first click on a nil target is not a double-click")
	}
}

func TestZeroValueTargetIsNotADoubleClick(t *testing.T) {
	if _, dbl := (Tracker[int]{}).Hit(0, time.Unix(100, 0)); dbl {
		t.Fatal("the first click on the zero-value target is not a double-click")
	}
}
