package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/secrets"
)

const checkKey = "sk-check-secret-value-123"

// listServer answers model listings with status and records each request.
type listServer struct {
	mu     sync.Mutex
	reqs   []*http.Request
	status int
}

func newListServer(t *testing.T, status int) (*listServer, *httptest.Server) {
	t.Helper()
	ls := &listServer{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.reqs = append(ls.reqs, r.Clone(context.Background()))
		ls.mu.Unlock()
		w.WriteHeader(ls.status)
		_, _ = w.Write([]byte(`{"error":"echoing ` + checkKey + `"}`))
	}))
	t.Cleanup(srv.Close)
	return ls, srv
}

func testEndpoints(base string) checkEndpoints {
	return checkEndpoints{
		googleAI: base + "/gai", anthropic: base + "/anth", openAI: base + "/oai",
		vertexToken: func(context.Context) error { return nil },
	}
}

func checkDeps(vars map[string]string) Deps {
	return Deps{Getenv: env(vars), Secrets: secrets.Resolver{Getenv: env(vars)}}
}

func TestCheckListsModelsWithTheKeyInAHeader(t *testing.T) {
	cases := []struct {
		cfg    config.AI
		vars   map[string]string
		path   string
		header string
		want   string
	}{
		{config.AI{Provider: "googleai"}, map[string]string{"GEMINI_API_KEY": checkKey}, "/gai/models", "X-Goog-Api-Key", checkKey},
		{config.AI{Provider: "anthropic"}, map[string]string{"ANTHROPIC_API_KEY": checkKey}, "/anth/models", "X-Api-Key", checkKey},
		{config.AI{Provider: "openai"}, map[string]string{"OPENAI_API_KEY": checkKey}, "/oai/models", "Authorization", "Bearer " + checkKey},
	}
	for _, tc := range cases {
		t.Run(tc.cfg.Provider, func(t *testing.T) {
			ls, srv := newListServer(t, http.StatusOK)
			if err := check(context.Background(), tc.cfg, checkDeps(tc.vars), testEndpoints(srv.URL), srv.Client()); err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(ls.reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(ls.reqs))
			}
			r := ls.reqs[0]
			if r.Method != http.MethodGet || r.URL.Path != tc.path {
				t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, tc.path)
			}
			if got := r.Header.Get(tc.header); got != tc.want {
				t.Errorf("%s = %q, want the key", tc.header, got)
			}
			if strings.Contains(r.URL.RawQuery, checkKey) {
				t.Error("the key is in the URL")
			}
		})
	}
}

func TestCheckBaseURLs(t *testing.T) {
	ls, srv := newListServer(t, http.StatusOK)
	ep := testEndpoints("http://unused.invalid")
	for _, tc := range []struct {
		cfg  config.AI
		path string
	}{
		{config.AI{Provider: "openai", BaseURL: srv.URL + "/v1/"}, "/v1/models"},
		{config.AI{Provider: "ollama", BaseURL: srv.URL}, "/api/tags"},
	} {
		ls.reqs = nil
		if err := check(context.Background(), tc.cfg, checkDeps(nil), ep, srv.Client()); err != nil {
			t.Fatalf("%s: %v", tc.cfg.Provider, err)
		}
		if len(ls.reqs) != 1 || ls.reqs[0].URL.Path != tc.path {
			t.Errorf("%s requests = %v, want one to %s", tc.cfg.Provider, ls.reqs, tc.path)
		}
		if h := ls.reqs[0].Header.Get("Authorization"); h != "" {
			t.Errorf("%s sent Authorization %q with no key", tc.cfg.Provider, h)
		}
	}
}

func TestCheckStatusClasses(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusUnauthorized:        assistant.ErrAuth,
		http.StatusForbidden:           assistant.ErrAuth,
		http.StatusTooManyRequests:     assistant.ErrRateLimit,
		http.StatusServiceUnavailable:  assistant.ErrUnavailable,
		http.StatusNotFound:            assistant.ErrProvider,
		http.StatusInternalServerError: assistant.ErrUnavailable,
	} {
		_, srv := newListServer(t, status)
		vars := map[string]string{"ANTHROPIC_API_KEY": checkKey}
		err := check(context.Background(), config.AI{Provider: "anthropic"}, checkDeps(vars), testEndpoints(srv.URL), srv.Client())
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: err = %v, want %v", status, err, want)
		}
		if err != nil && strings.Contains(err.Error(), checkKey) {
			t.Errorf("HTTP %d: error %q carries the key", status, err)
		}
	}
}

func TestCheckMissingKey(t *testing.T) {
	err := check(context.Background(), config.AI{Provider: "googleai"}, checkDeps(nil), testEndpoints("http://unused.invalid"), http.DefaultClient)
	if !errors.Is(err, assistant.ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

func TestCheckUnreachable(t *testing.T) {
	_, srv := newListServer(t, http.StatusOK)
	url, client := srv.URL, srv.Client() // its own transport: never the guarded default
	srv.Close()
	err := check(context.Background(), config.AI{Provider: "ollama", BaseURL: url}, checkDeps(nil), testEndpoints(url), client)
	if !errors.Is(err, assistant.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestCheckVertexUsesCredentials(t *testing.T) {
	ep := testEndpoints("http://unused.invalid")
	called := false
	ep.vertexToken = func(context.Context) error { called = true; return errors.New("no credentials") }
	err := check(context.Background(), config.AI{Provider: "vertexai"}, checkDeps(nil), ep, http.DefaultClient)
	if !called || !errors.Is(err, assistant.ErrAuth) {
		t.Errorf("called=%v err=%v, want the credentials check and ErrAuth", called, err)
	}
}

// TestKeyEnvMatchesConfig keeps config.AIKeyEnv, which `tobira doctor`
// reads offline, in step with the variables the providers read.
func TestKeyEnvMatchesConfig(t *testing.T) {
	for _, name := range config.AIProviders() {
		if got, want := providers[name].keyEnv, config.AIKeyEnv(name); !slices.Equal(got, want) {
			t.Errorf("%s: providers keyEnv = %v, config.AIKeyEnv = %v", name, got, want)
		}
	}
}

func TestCheckNeverFollowsRedirects(t *testing.T) {
	other, otherSrv := newListServer(t, http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherSrv.URL+"/stolen", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{"ANTHROPIC_API_KEY": checkKey}
	client := noRedirects(srv.Client())
	err := check(context.Background(), config.AI{Provider: "anthropic"}, checkDeps(vars), testEndpoints(srv.URL), client)
	if !errors.Is(err, assistant.ErrProvider) {
		t.Errorf("err = %v, want ErrProvider for a redirect", err)
	}
	if len(other.reqs) != 0 {
		t.Error("the redirect was followed: the key left the configured host")
	}
}

func TestCheckClientRefusesRedirects(t *testing.T) {
	c := checkClient()
	if c.CheckRedirect == nil {
		t.Fatal("the Check client follows redirects")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}
