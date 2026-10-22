package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/core/api"
	"github.com/firebase/genkit/go/genkit"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

// freePort returns a localhost port that is free right now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// An inherited GENKIT_ENV=dev must not turn on Genkit's dev mode: no
// reflection server listens and no telemetry export is configured.
func TestNewPinsGenkitProdMode(t *testing.T) {
	port := freePort(t)
	t.Setenv("GENKIT_ENV", "dev")
	t.Setenv("GENKIT_REFLECTION_PORT", port)
	t.Setenv("GENKIT_TELEMETRY_SERVER", "http://telemetry.tobira.test:4033")
	t.Setenv("GENKIT_REFLECTION_V2_SERVER", "ws://reflection.tobira.test:4100")
	if _, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil)}); err != nil {
		t.Fatal(err)
	}
	if got := api.CurrentEnvironment(); got != api.EnvironmentProd {
		t.Fatalf("Genkit environment = %q, want prod", got)
	}
	for _, k := range []string{"GENKIT_TELEMETRY_SERVER", "GENKIT_REFLECTION_V2_SERVER"} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s = %q, want it cleared", k, v)
		}
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("reflection port %s is bound: %v", port, err)
	}
	_ = l.Close()
}

// DevMode (only `tobira ai dev` sets it) leaves the Genkit environment as
// the user set it.
func TestDevModeLeavesEnvironment(t *testing.T) {
	t.Setenv("GENKIT_ENV", "")
	t.Setenv("GENKIT_TELEMETRY_SERVER", "")
	if _, err := New(context.Background(), config.AI{Provider: "ollama"}, Deps{Getenv: env(nil), DevMode: true}); err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("GENKIT_ENV"); v != "" {
		t.Fatalf("GENKIT_ENV = %q, want untouched", v)
	}
}

// The deferred init does not inherit the caller's cancellation, and an
// init failure caused by a context error is not remembered.
func TestDeferredInitIgnoresCallerCancel(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "vertexai", Project: "p"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	ga := a.(*genkitAssistant)
	calls := 0
	ga.initFn = func(ctx context.Context) (*genkit.Genkit, error) {
		calls++
		if ctx.Err() != nil {
			t.Errorf("init ctx is cancelled: %v", ctx.Err())
		}
		if calls == 1 {
			return nil, wrapProviderError(context.DeadlineExceeded, driver.Secret{})
		}
		return genkit.Init(context.Background(), genkit.WithPromptFS(noPromptFS), genkit.WithPromptDir(".")), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ga.genkit(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first init err = %v", err)
	}
	if g, err := ga.genkit(context.Background()); err != nil || g == nil {
		t.Fatalf("second init = %v, %v; want a retry that succeeds", g, err)
	}
	if _, err := ga.genkit(context.Background()); err != nil || calls != 2 {
		t.Fatalf("third call err=%v calls=%d; want the cached instance", err, calls)
	}
}

// A non-context init failure is remembered.
func TestDeferredInitCachesRealFailure(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "vertexai", Project: "p"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	ga := a.(*genkitAssistant)
	calls := 0
	ga.initFn = func(context.Context) (*genkit.Genkit, error) {
		calls++
		return nil, wrapProviderError(errors.New("no credentials"), driver.Secret{})
	}
	_, _ = ga.genkit(context.Background())
	_, err = ga.genkit(context.Background())
	if !errors.Is(err, assistant.ErrProvider) || calls != 1 {
		t.Fatalf("err=%v calls=%d; want the cached failure", err, calls)
	}
}

// initialised never waits on an init in progress: it reads a flag, not the
// init mutex, and reports true only once the init has finished.
func TestInitialisedDoesNotWaitOnInit(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "vertexai", Project: "p"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	ga := a.(*genkitAssistant)
	started, release := make(chan struct{}), make(chan struct{})
	ga.initFn = func(context.Context) (*genkit.Genkit, error) {
		close(started)
		<-release
		return nil, wrapProviderError(errors.New("no credentials"), driver.Secret{})
	}
	initDone := make(chan struct{})
	go func() {
		defer close(initDone)
		_, _ = ga.genkit(context.Background())
	}()
	<-started
	got := make(chan bool, 1)
	go func() { got <- ga.initialised() }()
	select {
	case v := <-got:
		if v {
			t.Error("initialised = true while the init is still running")
		}
	case <-time.After(5 * time.Second):
		t.Error("initialised blocked behind the init in progress")
	}
	close(release)
	<-initDone
	if !ga.initialised() {
		t.Fatal("initialised = false after the init finished")
	}
}

// errorStrings returns the message of err and of everything it unwraps to.
func errorStrings(err error) []string {
	if err == nil {
		return nil
	}
	out := []string{err.Error()}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		out = append(out, errorStrings(u.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			out = append(out, errorStrings(e)...)
		}
	}
	return out
}

func TestKeyRefFailureChainIsOpaque(t *testing.T) {
	deps := Deps{Getenv: env(nil), Secrets: secrets.Resolver{Resolve: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("open /secret/place/gemini.key: denied")
	}}}
	_, err := resolveKey(context.Background(), config.AI{Provider: "googleai", APIKey: "file:///secret/place/gemini.key"}, deps)
	if !errors.Is(err, assistant.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	for _, s := range errorStrings(err) {
		if strings.Contains(s, "secret/place") {
			t.Fatalf("error chain exposes the ref path: %q", s)
		}
	}
}

func TestProviderErrorRedactsJSONEscapedKey(t *testing.T) {
	key := "sk-a&b<c>/d"
	html, _ := json.Marshal(key)
	forms := []string{
		strings.Trim(string(html), `"`),                                // Go's HTML-safe escaping
		strings.ReplaceAll(strings.Trim(string(html), `"`), "/", `\/`), // and escaped slashes
		strings.ReplaceAll(key, "/", `\/`),
	}
	err := wrapProviderError(errors.New(`{"error":"bad key `+strings.Join(forms, " ")+`"}`), driver.NewSecret(key))
	for _, f := range forms {
		if strings.Contains(err.Error(), f) {
			t.Fatalf("error %q contains the JSON form %q", err, f)
		}
	}
}

// openai with a base_url (an OpenAI-compatible server) needs no key; the
// plugin gets a placeholder instead of panicking.
func TestOpenAIBaseURLWithoutKey(t *testing.T) {
	a, err := New(context.Background(), config.AI{Provider: "openai", BaseURL: "http://127.0.0.1:9/v1"}, Deps{Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model() != "openai/gpt-5" {
		t.Fatalf("Model = %q", a.Model())
	}
	if _, err := New(context.Background(), config.AI{Provider: "openai"}, Deps{Getenv: env(nil)}); !errors.Is(err, assistant.ErrAuth) {
		t.Fatalf("openai without base_url or key: err = %v, want ErrAuth", err)
	}
}

type panicPlugin struct{ msg string }

func (p panicPlugin) Name() string                      { return "panicky" }
func (p panicPlugin) Init(context.Context) []api.Action { panic(p.msg) }

// Every way a provider setup can panic becomes a redacted ErrProvider.
func TestProviderInitPanicsBecomeErrors(t *testing.T) {
	key := "sk-panic-key-123456"
	tests := map[string]provider{
		"plugin init": {defaultModel: "m", keyEnv: []string{"K"},
			plugin: func(config.AI, driver.Secret, func(string) string) api.Plugin {
				return panicPlugin{msg: "init failed for " + key}
			}},
		"plugin build": {defaultModel: "m", keyEnv: []string{"K"},
			plugin: func(config.AI, driver.Secret, func(string) string) api.Plugin { panic("build failed for " + key) }},
		"after init": {defaultModel: "m", keyEnv: []string{"K"},
			plugin:    func(config.AI, driver.Secret, func(string) string) api.Plugin { return &noopPlugin{} },
			afterInit: func(*genkit.Genkit, api.Plugin, config.AI) { panic("define failed for " + key) }},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			providers["panicky"] = p
			defer delete(providers, "panicky")
			_, err := New(context.Background(), config.AI{Provider: "panicky"}, Deps{Getenv: env(map[string]string{"K": key})})
			if !errors.Is(err, assistant.ErrProvider) {
				t.Fatalf("err = %v, want ErrProvider", err)
			}
			if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "panicky") {
				t.Fatalf("err %q must name the provider and not hold the key", err)
			}
		})
	}
}

type noopPlugin struct{}

func (*noopPlugin) Name() string                      { return "panicky" }
func (*noopPlugin) Init(context.Context) []api.Action { return nil }
