package sqlcomplete

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCache(t *testing.T) {
	f := newFake()
	c := NewCache(f, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if _, err := c.Collections(ctx, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls["coll"] != 1 {
		t.Fatalf("collections fetched %d times, want 1", f.calls["coll"])
	}
	if _, err := c.Collections(ctx, "other"); err != nil || f.calls["coll"] != 2 {
		t.Fatalf("a different namespace is a different key: calls=%d err=%v", f.calls["coll"], err)
	}

	now = now.Add(59 * time.Second)
	_, _ = c.Collections(ctx, "main")
	if f.calls["coll"] != 2 {
		t.Fatalf("entry must survive until the TTL: calls=%d", f.calls["coll"])
	}
	now = now.Add(2 * time.Second)
	_, _ = c.Collections(ctx, "main")
	if f.calls["coll"] != 3 {
		t.Fatalf("entry must expire after the TTL: calls=%d", f.calls["coll"])
	}

	f.colls["main"] = append(f.colls["main"], "fresh")
	if got, _ := c.Collections(ctx, "main"); len(got) != 3 {
		t.Fatalf("stale answer expected before Invalidate, got %v", got)
	}
	var inv Invalidator = c
	inv.Invalidate()
	if got, _ := c.Collections(ctx, "main"); len(got) != 4 {
		t.Fatalf("fresh answer expected after Invalidate, got %v", got)
	}
}

func TestCacheDoesNotCacheErrors(t *testing.T) {
	f := newFake()
	f.err = errors.New("boom")
	c := NewCache(f, 0)
	if _, err := c.Namespaces(context.Background()); err == nil {
		t.Fatal("want error")
	}
	f.err = nil
	if got, err := c.Namespaces(context.Background()); err != nil || len(got) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := c.Fields(context.Background(), "main", "people"); err != nil {
		t.Fatal(err)
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := NewCache(newFake(), time.Minute)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				_, _ = c.Fields(context.Background(), "main", "people")
				c.Invalidate()
			}
		}()
	}
	for range 8 {
		<-done
	}
}

// gatedCatalog answers Collections from data, but only after release is
// closed; started receives one value per call that begins.
type gatedCatalog struct {
	mu      sync.Mutex
	data    []string
	calls   int
	started chan struct{}
	release chan struct{}
}

func newGated(data ...string) *gatedCatalog {
	return &gatedCatalog{data: data, started: make(chan struct{}, 64), release: make(chan struct{})}
}

func (g *gatedCatalog) Namespaces(context.Context) ([]string, error) { return nil, nil }
func (g *gatedCatalog) Fields(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (g *gatedCatalog) Collections(ctx context.Context, _ string) ([]string, error) {
	g.mu.Lock()
	g.calls++
	snapshot := slices.Clone(g.data) // what the catalog said when the load began
	g.mu.Unlock()
	g.started <- struct{}{}
	select {
	case <-g.release:
		return snapshot, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gatedCatalog) set(data ...string) {
	g.mu.Lock()
	g.data = data
	g.mu.Unlock()
}

func (g *gatedCatalog) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestCacheInvalidateDuringLoadDoesNotStoreStale(t *testing.T) {
	g := newGated("old")
	c := NewCache(g, time.Minute)
	ctx := context.Background()

	done := make(chan []string)
	go func() {
		got, _ := c.Collections(ctx, "main")
		done <- got
	}()
	<-g.started // the load began and saw "old"
	g.set("new")
	c.Invalidate()
	close(g.release)
	if got := <-done; !slices.Equal(got, []string{"old"}) {
		t.Fatalf("the in-flight caller keeps its own answer, got %v", got)
	}
	got, err := c.Collections(ctx, "main")
	if err != nil || !slices.Equal(got, []string{"new"}) {
		t.Fatalf("after Invalidate the stale load must not be cached: got %v, %v", got, err)
	}
}

func TestCacheSingleflight(t *testing.T) {
	g := newGated("a")
	c := NewCache(g, time.Minute)
	const n = 8
	var ready, finished sync.WaitGroup
	ready.Add(n)
	finished.Add(n)
	results := make([][]string, n)
	for i := range n {
		go func() {
			defer finished.Done()
			ready.Done()
			results[i], _ = c.Collections(context.Background(), "main")
		}()
	}
	ready.Wait()
	<-g.started
	close(g.release)
	finished.Wait()
	if got := g.callCount(); got != 1 {
		t.Fatalf("concurrent misses must share one load, got %d", got)
	}
	for i, r := range results {
		if !slices.Equal(r, []string{"a"}) {
			t.Errorf("result %d = %v", i, r)
		}
	}
}

func TestCacheFollowerSurvivesLeaderCancel(t *testing.T) {
	g := newGated("a")
	c := NewCache(g, time.Minute)
	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.Collections(leaderCtx, "main")
		leaderErr <- err
	}()
	<-g.started
	followerRes := make(chan []string, 1)
	go func() {
		got, _ := c.Collections(context.Background(), "main")
		followerRes <- got
	}()
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	close(g.release) // lets the follower's own (retried) load finish
	if got := <-followerRes; !slices.Equal(got, []string{"a"}) {
		t.Fatalf("follower must not inherit the leader's cancellation, got %v", got)
	}
}

func TestCacheFollowerHonoursOwnContext(t *testing.T) {
	g := newGated("a")
	c := NewCache(g, time.Minute)
	go func() { _, _ = c.Collections(context.Background(), "main") }()
	<-g.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collections(ctx, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	close(g.release)
}

// goexitCatalog's first Collections call ends its goroutine with
// runtime.Goexit, the way a failed t.FailNow inside a loader would.
type goexitCatalog struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (g *goexitCatalog) Namespaces(context.Context) ([]string, error) { return nil, nil }
func (g *goexitCatalog) Fields(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (g *goexitCatalog) Collections(context.Context, string) ([]string, error) {
	g.mu.Lock()
	g.calls++
	first := g.calls == 1
	g.mu.Unlock()
	if first {
		g.started <- struct{}{}
		runtime.Goexit()
	}
	return []string{"ok"}, nil
}

func TestCacheGoexitInLoaderLeavesNoDeadFlight(t *testing.T) {
	g := &goexitCatalog{started: make(chan struct{}, 1)}
	c := NewCache(g, time.Minute)
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.Collections(context.Background(), "main")
	}()
	<-g.started
	followerDone := make(chan struct{})
	go func() {
		defer close(followerDone)
		_, _ = c.Collections(context.Background(), "main") // must not hang on the dead flight
	}()
	for _, ch := range []chan struct{}{leaderDone, followerDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a goroutine hung on a flight whose loader called runtime.Goexit")
		}
	}
	c.Invalidate() // whatever the follower stored, start clean
	got, err := c.Collections(context.Background(), "main")
	if err != nil || !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("after Goexit the key must load normally: %v, %v", got, err)
	}
}

type panicCatalog struct{ *gatedCatalog }

func (panicCatalog) Collections(context.Context, string) ([]string, error) { panic("kaboom") }

func TestCachePanicErrorCarriesStack(t *testing.T) {
	c := NewCache(panicCatalog{newGated()}, time.Minute)
	_, err := c.Collections(context.Background(), "main")
	if err == nil || !strings.Contains(err.Error(), "kaboom") || !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("err = %v, want the panic value and a stack", err)
	}
	if len(err.Error()) > 5000 {
		t.Fatalf("stack not truncated: %d bytes", len(err.Error()))
	}
}
