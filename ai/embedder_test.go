package ai

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// No ai.embedder means no embedder: nil, nil (a nil interface).
func TestNewEmbedderEmpty(t *testing.T) {
	for _, name := range []string{"", "   "} {
		e, err := NewEmbedder(context.Background(), config.AI{Provider: "googleai", Embedder: name}, Deps{})
		if err != nil || e != nil {
			t.Fatalf("NewEmbedder(%q) = %v, %v; want nil, nil", name, e, err)
		}
	}
}

// ai.embedder is provider/model, or a model of ai.provider.
func TestNewEmbedderParsesProvider(t *testing.T) {
	for _, tc := range []struct {
		cfg          config.AI
		prov, model  string
		keepsKeyBase bool
	}{
		{config.AI{Provider: "anthropic", Embedder: "googleai/text-embedding-005", APIKey: "env:ANT", BaseURL: "http://x"}, "googleai", "text-embedding-005", false},
		{config.AI{Provider: "openai", Embedder: "text-embedding-3-small", APIKey: "env:OAI"}, "openai", "text-embedding-3-small", true},
		{config.AI{Provider: "ollama", Embedder: "ollama/nomic-embed-text", BaseURL: "http://127.0.0.1:9"}, "ollama", "nomic-embed-text", true},
		{config.AI{Provider: "ollama", Embedder: "library/nomic-embed-text"}, "ollama", "library/nomic-embed-text", true},
	} {
		e, err := NewEmbedder(context.Background(), tc.cfg, Deps{Getenv: env(nil)})
		if err != nil {
			t.Fatalf("%q: %v", tc.cfg.Embedder, err)
		}
		ge := e.(*genkitEmbedder)
		if ge.cfg.Provider != tc.prov || ge.cfg.Model != tc.model {
			t.Errorf("%q: provider %q model %q, want %q %q", tc.cfg.Embedder, ge.cfg.Provider, ge.cfg.Model, tc.prov, tc.model)
		}
		// ai.api_key and ai.base_url belong to ai.provider: another
		// provider's embedder does not get them.
		if kept := ge.cfg.APIKey == tc.cfg.APIKey && ge.cfg.BaseURL == tc.cfg.BaseURL; kept != tc.keepsKeyBase {
			t.Errorf("%q: api_key/base_url kept = %v, want %v", tc.cfg.Embedder, kept, tc.keepsKeyBase)
		}
	}
}

// A provider with no embedders, or a missing model, is refused up front.
func TestNewEmbedderRefuses(t *testing.T) {
	for _, cfg := range []config.AI{
		{Provider: "googleai", Embedder: "anthropic/claude"},
		{Provider: "anthropic", Embedder: "voyage-3"},
		{Provider: "googleai", Embedder: "googleai/"},
	} {
		e, err := NewEmbedder(context.Background(), cfg, Deps{Getenv: env(nil)})
		if !errors.Is(err, assistant.ErrProvider) || e != nil {
			t.Errorf("%+v: %v, %v; want ErrProvider and a nil interface", cfg, e, err)
		}
	}
}

// NewEmbedder reads no secret and initialises nothing: the key is resolved
// and Genkit started on the first Embed, so a command that never searches
// by text pays nothing. A missing key is then a redacted ErrAuth.
func TestNewEmbedderIsLazy(t *testing.T) {
	var resolved atomic.Int32
	deps := Deps{Getenv: env(nil), Secrets: secrets.Resolver{Resolve: func(context.Context, string) ([]byte, error) {
		resolved.Add(1)
		return nil, errors.New("open /vault/gemini.key: denied")
	}}}
	e, err := NewEmbedder(context.Background(), config.AI{Provider: "googleai", Embedder: "text-embedding-005", APIKey: "file:///vault/gemini.key"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Load() != 0 || e.(*genkitEmbedder).initialised() {
		t.Fatal("NewEmbedder resolved the key or initialised Genkit")
	}
	_, err = e.Embed(context.Background(), []string{"x"})
	if !errors.Is(err, assistant.ErrAuth) || strings.Contains(err.Error(), "vault") {
		t.Fatalf("first Embed: %v, want a redacted ErrAuth", err)
	}
	if resolved.Load() != 1 {
		t.Fatalf("key resolved %d times", resolved.Load())
	}
	// The failure is remembered, not retried on every search.
	_, _ = e.Embed(context.Background(), []string{"x"})
	if resolved.Load() != 1 {
		t.Fatalf("key resolved %d times after a second Embed", resolved.Load())
	}
}

// Each provider with embedders initialises and resolves its embedder with
// a fake key and no network (the embedder is never called).
func TestEmbedderInitPerProvider(t *testing.T) {
	keys := map[string]string{"GEMINI_API_KEY": "fake-gemini", "OPENAI_API_KEY": "fake-oai"}
	for _, cfg := range []config.AI{
		{Provider: "googleai", Embedder: "googleai/text-embedding-005"},
		{Provider: "openai", Embedder: "openai/text-embedding-3-small"},
		{Provider: "openai", Embedder: "my-compatible-embedder", BaseURL: "http://127.0.0.1:9/v1"},
		{Provider: "ollama", Embedder: "ollama/nomic-embed-text"},
	} {
		t.Run(cfg.Embedder, func(t *testing.T) {
			e, err := NewEmbedder(context.Background(), cfg, Deps{Getenv: env(keys)})
			if err != nil {
				t.Fatal(err)
			}
			emb, _, err := e.(*genkitEmbedder).ready(context.Background())
			if err != nil || emb == nil {
				t.Fatalf("ready = %v, %v", emb, err)
			}
		})
	}
}

// testEmbedder returns a genkitEmbedder over a Genkit embedder defined in a
// tool-only instance, which fn implements.
func testEmbedder(t *testing.T, key string, fn gai.EmbedderFunc) *genkitEmbedder {
	t.Helper()
	g, err := NewToolHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	emb := genkit.DefineEmbedderAction(g, "test/"+strings.ReplaceAll(t.Name(), "/", "_"), nil,
		func(ctx context.Context, req *gai.EmbedRequest, _ struct{}) (*gai.EmbedResponse, error) {
			return fn(ctx, req)
		})
	e, err := NewEmbedder(context.Background(), config.AI{Provider: "ollama", Embedder: "ollama/test"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	ge := e.(*genkitEmbedder)
	ge.initFn = func(context.Context) (gai.Embedder, driver.Secret, error) { return emb, driver.NewSecret(key), nil }
	return ge
}

// lengthVectors embeds each text as [len(text), 1] and counts its calls.
func lengthVectors(calls *atomic.Int32, batch *atomic.Int32) gai.EmbedderFunc {
	return func(_ context.Context, req *gai.EmbedRequest) (*gai.EmbedResponse, error) {
		calls.Add(1)
		if n := int32(len(req.Input)); n > batch.Load() {
			batch.Store(n)
		}
		resp := &gai.EmbedResponse{}
		for _, d := range req.Input {
			var text strings.Builder
			for _, p := range d.Content {
				text.WriteString(p.Text)
			}
			resp.Embeddings = append(resp.Embeddings, &gai.Embedding{Embedding: []float32{float32(text.Len()), 1}})
		}
		return resp, nil
	}
}

// Embed returns one vector per text in order, batches large inputs, and
// learns the dimension from the first answer.
func TestEmbedderEmbeds(t *testing.T) {
	var calls, batch atomic.Int32
	e := testEmbedder(t, "", lengthVectors(&calls, &batch))
	if e.Dimensions() != 0 {
		t.Fatalf("Dimensions before any Embed = %d, want 0 (unknown)", e.Dimensions())
	}
	got, err := e.Embed(context.Background(), []string{"a", "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][0] != 3 || e.Dimensions() != 2 {
		t.Fatalf("Embed = %v, Dimensions = %d", got, e.Dimensions())
	}
	texts := make([]string, 150)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	calls.Store(0)
	got, err = e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range got {
		if int(v[0]) != i+1 {
			t.Fatalf("vector %d = %v: order lost", i, v)
		}
	}
	if calls.Load() < 2 || batch.Load() > embedBatch {
		t.Fatalf("%d calls, largest batch %d: want batches of at most %d", calls.Load(), batch.Load(), embedBatch)
	}
	if out, err := e.Embed(context.Background(), nil); err != nil || len(out) != 0 {
		t.Fatalf("Embed(nil) = %v, %v", out, err)
	}
}

// Provider errors are classified and redacted against the key; a wrong
// number of vectors or ragged sizes are errors, not silent garbage.
func TestEmbedderErrors(t *testing.T) {
	const key = "sk-embed-secret-123"
	e := testEmbedder(t, key, func(context.Context, *gai.EmbedRequest) (*gai.EmbedResponse, error) {
		return nil, errors.New("401 bad key " + key)
	})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || strings.Contains(err.Error(), key) || !errors.Is(err, assistant.ErrProvider) {
		t.Fatalf("err = %v, want a redacted ErrProvider", err)
	}
	short := testEmbedder(t, "", func(context.Context, *gai.EmbedRequest) (*gai.EmbedResponse, error) {
		return &gai.EmbedResponse{Embeddings: []*gai.Embedding{{Embedding: []float32{1}}}}, nil
	})
	if _, err := short.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("1 vector for 2 texts was accepted")
	}
	ragged := testEmbedder(t, "", func(_ context.Context, req *gai.EmbedRequest) (*gai.EmbedResponse, error) {
		return &gai.EmbedResponse{Embeddings: []*gai.Embedding{{Embedding: []float32{1, 2}}, {Embedding: []float32{1}}}}, nil
	})
	if _, err := ragged.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("vectors of different sizes were accepted")
	}
}

// Concurrent first Embeds initialise once.
func TestEmbedderConcurrentInit(t *testing.T) {
	var calls, batch atomic.Int32
	e := testEmbedder(t, "", lengthVectors(&calls, &batch))
	inner := e.initFn
	var inits atomic.Int32
	e.initFn = func(ctx context.Context) (gai.Embedder, driver.Secret, error) {
		inits.Add(1)
		return inner(ctx)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := e.Embed(context.Background(), []string{"q"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if inits.Load() != 1 {
		t.Fatalf("%d inits", inits.Load())
	}
}

// A caller whose context ends while the first init is still running (a
// slow key ref, a keyring prompt) gets its context error at once, and so
// does a concurrent caller; the init carries on and later callers use it.
func TestEmbedderInitCancellable(t *testing.T) {
	var calls, batch atomic.Int32
	e := testEmbedder(t, "", lengthVectors(&calls, &batch))
	inner := e.initFn
	release := make(chan struct{})
	var inits atomic.Int32
	e.initFn = func(ctx context.Context) (gai.Embedder, driver.Secret, error) {
		inits.Add(1)
		<-release
		return inner(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := e.Embed(ctx, []string{"q"}); errs <- err }()
	}
	cancel()
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Embed blocked on an init its caller no longer waits for")
		}
	}
	close(release)
	if _, err := e.Embed(context.Background(), []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if inits.Load() != 1 {
		t.Fatalf("%d inits, want the first one reused", inits.Load())
	}
}

// Init is bounded: when it outlives initTimeout (a key ref that hangs) the
// error is returned and not remembered, so the next search tries again.
func TestEmbedderInitTimeoutIsRetried(t *testing.T) {
	var calls, batch atomic.Int32
	e := testEmbedder(t, "", lengthVectors(&calls, &batch))
	inner := e.initFn
	e.initTimeout = 20 * time.Millisecond
	var inits atomic.Int32
	e.initFn = func(ctx context.Context) (gai.Embedder, driver.Secret, error) {
		if inits.Add(1) == 1 {
			<-ctx.Done()
			// resolveKey drops the context error from its chain, as here.
			return nil, driver.Secret{}, errors.New("authentication failed: key ref timed out")
		}
		return inner(ctx)
	}
	if _, err := e.Embed(context.Background(), []string{"q"}); err == nil {
		t.Fatal("a timed-out init succeeded")
	}
	if _, err := e.Embed(context.Background(), []string{"q"}); err != nil {
		t.Fatalf("second Embed after a timed-out init: %v", err)
	}
	if inits.Load() != 2 {
		t.Fatalf("%d inits, want a retry", inits.Load())
	}
	if (&genkitEmbedder{}).timeout() != embedInitTimeout || embedInitTimeout != 30*time.Second {
		t.Fatalf("default init bound = %v", embedInitTimeout)
	}
}

// Like the assistant, the embedder leaves Genkit's environment alone in dev
// mode (`tobira ai dev`) and pins prod otherwise.
func TestEmbedderDevModeLeavesEnvironment(t *testing.T) {
	for _, dev := range []bool{true, false} {
		t.Setenv("GENKIT_ENV", "")
		t.Setenv("GENKIT_TELEMETRY_SERVER", "http://telemetry.tobira.test")
		e, err := NewEmbedder(context.Background(), config.AI{Provider: "ollama", Embedder: "ollama/nomic-embed-text"}, Deps{Getenv: env(nil), DevMode: dev})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.(*genkitEmbedder).ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		genv, tel := os.Getenv("GENKIT_ENV"), os.Getenv("GENKIT_TELEMETRY_SERVER")
		if dev && (genv != "" || tel == "") {
			t.Errorf("dev mode: GENKIT_ENV=%q telemetry=%q, want both untouched", genv, tel)
		}
		if !dev && (genv != "prod" || tel != "") {
			t.Errorf("normal mode: GENKIT_ENV=%q telemetry=%q, want prod and unset", genv, tel)
		}
	}
}
