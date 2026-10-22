package ai

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gai "github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/compat_oai/openai"
	"github.com/firebase/genkit/go/plugins/ollama"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
)

// embedBatch caps the texts sent in one embed request; larger inputs are
// split, since providers limit the batch size.
const embedBatch = 64

// ollamaDimensionsPlaceholder is the dimension declared to the ollama
// plugin, which requires one up front; it is metadata only and never
// checked against the vectors the server returns.
const ollamaDimensionsPlaceholder = 1

// providerEmbedders resolves an embedder on an initialised Genkit instance,
// per provider. A provider missing here (anthropic) has no embedders.
var providerEmbedders = map[string]func(g *genkit.Genkit, p api.Plugin, model string) gai.Embedder{
	"googleai": lookupEmbedder("googleai"),
	"vertexai": lookupEmbedder("vertexai"),
	"openai": func(g *genkit.Genkit, p api.Plugin, model string) gai.Embedder {
		if e := genkit.LookupEmbedder(g, "openai/"+model); e != nil {
			return e
		}
		// A model the plugin does not list (an OpenAI-compatible server's)
		// is defined directly; the plugin offers no other way.
		return p.(*openai.OpenAI).DefineEmbedder(model, nil) //nolint:staticcheck // see above
	},
	"ollama": func(g *genkit.Genkit, p api.Plugin, model string) gai.Embedder {
		return p.(*ollama.Ollama).DefineEmbedder(g, model, ollamaDimensionsPlaceholder, nil)
	},
}

// lookupEmbedder resolves provider/model through the plugin's dynamic
// action resolution.
func lookupEmbedder(provider string) func(*genkit.Genkit, api.Plugin, string) gai.Embedder {
	return func(g *genkit.Genkit, _ api.Plugin, model string) gai.Embedder {
		return genkit.LookupEmbedder(g, provider+"/"+model)
	}
}

// genkitEmbedder implements driver.Embedder on a Genkit embedder.
type genkitEmbedder struct {
	cfg  config.AI // Provider and Model name the embedder
	deps Deps

	// initFn resolves the key, starts Genkit and returns the embedder; nil
	// means initEmbedder. Its context ends after initTimeout. Tests
	// replace it.
	initFn      func(context.Context) (gai.Embedder, driver.Secret, error)
	initTimeout time.Duration // 0 means embedInitTimeout
	mu          sync.Mutex    // guards emb, key, initErr and flight; never held during init
	emb         gai.Embedder
	key         driver.Secret
	initErr     error
	flight      *initFlight // the init in progress, nil when none
	inited      atomic.Bool
	dims        atomic.Int64
}

// embedInitTimeout bounds one init: resolving the key (which may wait on a
// keyring or a command ref) and starting Genkit.
const embedInitTimeout = 30 * time.Second

// initFlight is one init shared by every caller waiting on it.
type initFlight struct {
	done chan struct{} // closed once emb, key and err are set
	emb  gai.Embedder
	key  driver.Secret
	err  error
}

// NewEmbedder builds the host embedder for vector search from ai.embedder:
// "provider/model" (googleai/text-embedding-005, ollama/nomic-embed-text,
// openai/text-embedding-3-small, vertexai/…), or a bare model of
// ai.provider. It returns nil, nil when ai.embedder is empty.
//
// It reads no secret and touches no network: on the first Embed the key is
// resolved as for the assistant (ai.api_key, else the provider's variable)
// and a private Genkit instance pinned to prod is started with the
// provider's plugin (in dev mode, deps.DevMode, Genkit's environment is
// left as the user set it, as for the assistant). The init is bounded by
// 30 seconds and a caller's cancellation never waits on it. ai.api_key and
// ai.base_url apply only when the
// embedder's provider is ai.provider; another provider takes its key from
// its environment variable. A provider without embedders (anthropic) or a
// missing model is an assistant.ErrProvider here. Errors are redacted
// against the key and wrap the assistant sentinels.
func NewEmbedder(_ context.Context, cfg config.AI, deps Deps) (driver.Embedder, error) {
	name := strings.TrimSpace(cfg.Embedder)
	if name == "" {
		return nil, nil
	}
	prov, model := cfg.Provider, name
	if i := strings.IndexByte(name, '/'); i > 0 {
		if _, ok := lookupProvider(name[:i]); ok {
			prov, model = name[:i], name[i+1:]
		}
	}
	if _, ok := lookupProvider(prov); !ok {
		return nil, fmt.Errorf("%w: ai.embedder: unknown provider", assistant.ErrProvider)
	}
	if providerEmbedders[prov] == nil {
		return nil, fmt.Errorf("%w: ai.embedder: %s has no embedders; name one as provider/model, such as googleai/text-embedding-005", assistant.ErrProvider, prov)
	}
	if model == "" {
		return nil, fmt.Errorf("%w: ai.embedder names no model", assistant.ErrProvider)
	}
	ecfg := cfg
	if prov != cfg.Provider {
		ecfg.APIKey, ecfg.BaseURL = "", ""
	}
	ecfg.Provider, ecfg.Model = prov, model
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	return &genkitEmbedder{cfg: ecfg, deps: deps}, nil
}

// Dimensions returns the vector size seen in the last answer, 0 until the
// first Embed succeeds.
func (e *genkitEmbedder) Dimensions() int { return int(e.dims.Load()) }

// initialised reports whether init has run (successfully or not).
func (e *genkitEmbedder) initialised() bool { return e.inited.Load() }

// timeout is the bound of one init.
func (e *genkitEmbedder) timeout() time.Duration {
	if e.initTimeout > 0 {
		return e.initTimeout
	}
	return embedInitTimeout
}

// ready returns the embedder and key, initialising on first use. The init
// runs once, in the background and detached from any caller's
// cancellation, bounded by timeout(); every caller waits for it only as
// long as its own ctx allows. A failed init is remembered, except one that
// ran out of time, which the next caller retries.
func (e *genkitEmbedder) ready(ctx context.Context) (gai.Embedder, driver.Secret, error) {
	e.mu.Lock()
	if e.emb != nil || e.initErr != nil {
		defer e.mu.Unlock()
		return e.emb, e.key, e.initErr
	}
	f := e.flight
	if f == nil {
		f = &initFlight{done: make(chan struct{})}
		e.flight = f
		go e.runInit(context.WithoutCancel(ctx), f)
	}
	e.mu.Unlock()
	select {
	case <-f.done:
		return f.emb, f.key, f.err
	case <-ctx.Done():
		return nil, driver.Secret{}, ctx.Err()
	}
}

// runInit performs one init and publishes it to f and, unless it timed
// out, to e.
func (e *genkitEmbedder) runInit(ctx context.Context, f *initFlight) {
	ictx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	initFn := e.initFn
	if initFn == nil {
		initFn = e.initEmbedder
	}
	emb, key, err := initFn(ictx)
	if err == nil && emb == nil {
		err = fmt.Errorf("%w: no embedder", assistant.ErrProvider)
	}
	timedOut := err != nil && ictx.Err() != nil
	if timedOut {
		err = fmt.Errorf("%w: the embedder did not start within %v: %w", assistant.ErrUnavailable, e.timeout(), err)
	}
	e.mu.Lock()
	e.flight = nil
	if !timedOut {
		e.emb, e.key, e.initErr = emb, key, err
		e.inited.Store(true)
	}
	e.mu.Unlock()
	f.emb, f.key, f.err = emb, key, err
	close(f.done)
}

// initEmbedder resolves the key, starts a private Genkit instance with the
// provider's plugin and resolves the embedder. Genkit plugins report setup
// problems by panicking; a panic becomes a redacted ErrProvider.
func (e *genkitEmbedder) initEmbedder(ctx context.Context) (emb gai.Embedder, key driver.Secret, err error) {
	key, err = resolveKey(ctx, e.cfg, e.deps)
	if err != nil {
		return nil, driver.Secret{}, err
	}
	defer func() {
		if r := recover(); r != nil {
			emb, err = nil, wrapProviderError(fmt.Errorf("initialise %s embedder: %v", e.cfg.Provider, r), key)
		}
	}()
	p, _ := lookupProvider(e.cfg.Provider)
	plugin := p.plugin(e.cfg, key, e.deps.Getenv)
	// Genkit keeps the Init context for the instance's life: it must not
	// end with the init bound.
	g := initGenkitFor(context.WithoutCancel(ctx), plugin, e.deps.DevMode)
	if emb = providerEmbedders[e.cfg.Provider](g, plugin, e.cfg.Model); emb == nil {
		return nil, key, fmt.Errorf("%w: embedder %s/%s is not available", assistant.ErrProvider, e.cfg.Provider, e.cfg.Model)
	}
	e.deps.Logger.Debug("embedder configured", "embedder", e.cfg.Provider+"/"+e.cfg.Model)
	return emb, key, nil
}

// initGenkitFor runs genkit.Init with plugin and no prompts, pinned to
// prod unless dev (Deps.DevMode, as for the assistant).
func initGenkitFor(ctx context.Context, plugin api.Plugin, dev bool) *genkit.Genkit {
	genkitEnvMu.Lock()
	defer genkitEnvMu.Unlock()
	if !dev {
		pinGenkitProd()
	}
	return genkit.Init(ctx,
		genkit.WithPlugins(plugin),
		genkit.WithPromptFS(noPromptFS),
		genkit.WithPromptDir("."),
	)
}

// Embed returns one vector per text, in order, sending at most embedBatch
// texts per request. Every vector has the same size.
func (e *genkitEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	emb, key, err := e.ready(ctx)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatch {
		batch := texts[start:min(start+embedBatch, len(texts))]
		docs := make([]*gai.Document, len(batch))
		for i, s := range batch {
			docs[i] = gai.DocumentFromText(s, nil)
		}
		resp, err := emb.Embed(ctx, &gai.EmbedRequest{Input: docs})
		if err != nil {
			return nil, wrapProviderError(err, key)
		}
		if resp == nil || len(resp.Embeddings) != len(batch) {
			n := 0
			if resp != nil {
				n = len(resp.Embeddings)
			}
			return nil, fmt.Errorf("%w: the embedder returned %d vectors for %d texts", assistant.ErrProvider, n, len(batch))
		}
		for _, v := range resp.Embeddings {
			if v == nil || len(v.Embedding) == 0 || (len(out) > 0 && len(v.Embedding) != len(out[0])) {
				return nil, fmt.Errorf("%w: the embedder returned vectors of different sizes", assistant.ErrProvider)
			}
			out = append(out, v.Embedding)
		}
	}
	e.dims.Store(int64(len(out[0])))
	return out, nil
}

var _ driver.Embedder = (*genkitEmbedder)(nil)
