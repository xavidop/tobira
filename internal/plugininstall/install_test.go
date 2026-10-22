package plugininstall_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xavidop/tobira/internal/plugininstall"
)

var payload = []byte("#!/bin/sh\necho not a real plugin\n")

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// pluginsDir is a fresh, not yet created plugins directory.
func pluginsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "tobira", "plugins")
}

// serve answers every request with body, and records the User-Agent.
func serve(t *testing.T, body []byte) (*httptest.Server, *atomic.Value) {
	t.Helper()
	ua := &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, ua
}

// entries lists dir's file names ("" for a missing dir).
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func install(o plugininstall.Options) (plugininstall.Result, error) {
	if o.UserAgent == "" {
		o.UserAgent = "tobira/test"
	}
	return plugininstall.Install(context.Background(), o)
}

func TestInstallFromURL(t *testing.T) {
	srv, ua := serve(t, payload)
	dir := pluginsDir(t)
	res, err := install(plugininstall.Options{
		Source: srv.URL + "/releases/tobira-driver-demo?token=abc", SHA256: strings.ToUpper(sum(payload)), Dir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "tobira-driver-demo")
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if res.Name != "demo" || res.Path != want || res.Bytes != int64(len(payload)) {
		t.Errorf("Result = %+v, want demo at %s", res, want)
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("installed file = %q, %v", got, err)
	}
	if got := ua.Load(); got != "tobira/test" {
		t.Errorf("User-Agent = %v, want tobira/test", got)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(want)
		if st.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want 0755", st.Mode().Perm())
		}
		dst, _ := os.Stat(dir)
		if dst.Mode().Perm() != 0o700 {
			t.Errorf("plugins dir mode = %v, want 0700", dst.Mode().Perm())
		}
	}
	if es := entries(t, dir); len(es) != 1 {
		t.Errorf("plugins dir holds %v, want only the plugin", es)
	}
}

func TestInstallBadChecksumLeavesNothing(t *testing.T) {
	srv, _ := serve(t, payload)
	dir := pluginsDir(t)
	_, err := install(plugininstall.Options{
		Source: srv.URL + "/tobira-driver-demo", SHA256: sum([]byte("something else")), Dir: dir,
	})
	if !errors.Is(err, plugininstall.ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	if !strings.Contains(err.Error(), sum(payload)) {
		t.Errorf("error %q does not name the checksum it got", err)
	}
	if es := entries(t, dir); len(es) != 0 {
		t.Errorf("plugins dir holds %v after a bad checksum, want nothing (no temp files)", es)
	}
}

func TestInstallChecksumFormat(t *testing.T) {
	srv, _ := serve(t, payload)
	good := sum(payload)
	for name, s := range map[string]string{
		"missing":   "",
		"short":     good[:63],
		"long":      good + "0",
		"not hex":   "z" + good[1:],
		"spaces":    " " + good[1:],
		"prefixed":  "sha256:" + good,
		"truncated": good[:32],
	} {
		t.Run(name, func(t *testing.T) {
			dir := pluginsDir(t)
			_, err := install(plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: s, Dir: dir})
			if !errors.Is(err, plugininstall.ErrInput) {
				t.Fatalf("err = %v, want ErrInput", err)
			}
			if es := entries(t, dir); len(es) != 0 {
				t.Errorf("plugins dir holds %v", es)
			}
		})
	}
}

func TestInstallSizeCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 1000)
	t.Run("streamed", func(t *testing.T) {
		// No Content-Length: the cap applies while streaming.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i < 10; i++ {
				_, _ = w.Write(big[:100])
				w.(http.Flusher).Flush()
			}
		}))
		t.Cleanup(srv.Close)
		dir := pluginsDir(t)
		_, err := install(plugininstall.Options{
			Source: srv.URL + "/tobira-driver-demo", SHA256: sum(big), Dir: dir, MaxBytes: 999,
		})
		if !errors.Is(err, plugininstall.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
		if es := entries(t, dir); len(es) != 0 {
			t.Errorf("plugins dir holds %v", es)
		}
	})
	t.Run("announced", func(t *testing.T) {
		var sent atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			n, _ := w.Write(big)
			sent.Add(int64(n))
		}))
		t.Cleanup(srv.Close)
		_, err := install(plugininstall.Options{
			Source: srv.URL + "/tobira-driver-demo", SHA256: sum(big), Dir: pluginsDir(t), MaxBytes: 999,
		})
		if !errors.Is(err, plugininstall.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})
	t.Run("exactly at the cap", func(t *testing.T) {
		srv, _ := serve(t, big)
		_, err := install(plugininstall.Options{
			Source: srv.URL + "/tobira-driver-demo", SHA256: sum(big), Dir: pluginsDir(t), MaxBytes: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("local file", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "tobira-driver-demo")
		if err := os.WriteFile(src, big, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := install(plugininstall.Options{Source: src, SHA256: sum(big), Dir: pluginsDir(t), MaxBytes: 999})
		if !errors.Is(err, plugininstall.ErrTooLarge) {
			t.Fatalf("err = %v, want ErrTooLarge", err)
		}
	})
	if plugininstall.MaxSize != 256<<20 {
		t.Errorf("MaxSize = %d, want 256 MiB", plugininstall.MaxSize)
	}
}

func TestInstallRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	})
	mux.HandleFunc("/ftp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.com/tobira-driver-demo", http.StatusFound)
	})
	mux.HandleFunc("/hop/", func(w http.ResponseWriter, r *http.Request) {
		// /hop/N redirects to /hop/N-1, and /hop/0 serves the payload.
		n := strings.TrimPrefix(r.URL.Path, "/hop/")
		if n == "0" {
			_, _ = w.Write(payload)
			return
		}
		http.Redirect(w, r, "/hop/"+string(rune(n[0]-1)), http.StatusFound)
	})
	mux.HandleFunc("/missing", http.NotFound)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/file", "/ftp"} {
		t.Run(path, func(t *testing.T) {
			dir := pluginsDir(t)
			_, err := install(plugininstall.Options{Source: srv.URL + path, Name: "tobira-driver-demo", SHA256: sum(payload), Dir: dir})
			if !errors.Is(err, plugininstall.ErrDownload) || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("err = %v, want a refused redirect", err)
			}
			if es := entries(t, dir); len(es) != 0 {
				t.Errorf("plugins dir holds %v", es)
			}
		})
	}
	t.Run("five hops", func(t *testing.T) {
		_, err := install(plugininstall.Options{Source: srv.URL + "/hop/5", Name: "tobira-driver-demo", SHA256: sum(payload), Dir: pluginsDir(t)})
		if err != nil {
			t.Fatalf("5 redirects: %v", err)
		}
	})
	t.Run("six hops", func(t *testing.T) {
		_, err := install(plugininstall.Options{Source: srv.URL + "/hop/6", Name: "tobira-driver-demo", SHA256: sum(payload), Dir: pluginsDir(t)})
		if !errors.Is(err, plugininstall.ErrDownload) || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("6 redirects: err = %v, want too many redirects", err)
		}
	})
	t.Run("404", func(t *testing.T) {
		_, err := install(plugininstall.Options{Source: srv.URL + "/missing", Name: "tobira-driver-demo", SHA256: sum(payload), Dir: pluginsDir(t)})
		if !errors.Is(err, plugininstall.ErrDownload) || !strings.Contains(err.Error(), "404") {
			t.Fatalf("err = %v, want a 404 download error", err)
		}
	})
}

func TestInstallRefusesOtherSchemes(t *testing.T) {
	for _, src := range []string{"file:///etc/passwd", "ftp://example.com/tobira-driver-demo", "javascript://x/tobira-driver-demo"} {
		_, err := install(plugininstall.Options{Source: src, SHA256: sum(payload), Dir: pluginsDir(t)})
		if !errors.Is(err, plugininstall.ErrInput) {
			t.Errorf("%s: err = %v, want ErrInput", src, err)
		}
	}
}

func TestInstallNames(t *testing.T) {
	srv, _ := serve(t, payload)
	for _, name := range []string{
		"../x", "a/b", `a\b`, "tobira-driver-../..", "tobira-driver-a/../../evil", "tobira-driver-x\x00",
		"..", "tobira-driver-", "tobira-driver-Upper", "tobira-driver-under_score", "tobira-driver-a.b",
		"tobira-driver-" + strings.Repeat("a", 64), "tobira-driver--dash",
		"tobira-driver-\uff41", // fullwidth a
		"tobira-driver-\u0430", // Cyrillic a
		"tobira-driver-demo.",  // trailing dot
		"tobira-driver-demo ",  // trailing space
		"tobira-driver-demo\n", // trailing newline
		"tobira-driver-de mo",  // inner space
		"tobira-driver-demo\r", // trailing carriage return
		"tobira-driver-demo\t", // trailing tab
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "plugins")
			_, err := install(plugininstall.Options{Source: srv.URL + "/tobira-driver-ok", Name: name, SHA256: sum(payload), Dir: dir})
			if !errors.Is(err, plugininstall.ErrInput) {
				t.Fatalf("--name %q: err = %v, want ErrInput", name, err)
			}
			if es := entries(t, root); len(es) != 0 {
				t.Errorf("--name %q wrote %v", name, es)
			}
		})
	}
	t.Run("from the URL", func(t *testing.T) {
		for _, path := range []string{"/x/..%2F..%2Fevil", "/tobira-driver-bad_name", "/", "/dl/plugin.tar.gz"} {
			_, err := install(plugininstall.Options{Source: srv.URL + path, SHA256: sum(payload), Dir: pluginsDir(t)})
			if !errors.Is(err, plugininstall.ErrInput) {
				t.Errorf("URL path %q: err = %v, want ErrInput", path, err)
			}
		}
	})
	t.Run("bare driver name", func(t *testing.T) {
		dir := pluginsDir(t)
		res, err := install(plugininstall.Options{Source: srv.URL + "/download", Name: "demo", SHA256: sum(payload), Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if res.Name != "demo" || filepath.Dir(res.Path) != dir || !strings.HasPrefix(filepath.Base(res.Path), "tobira-driver-demo") {
			t.Errorf("Result = %+v", res)
		}
	})
}

func TestInstallExistingFile(t *testing.T) {
	srv, _ := serve(t, payload)
	dir := pluginsDir(t)
	o := plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: dir}
	res, err := install(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(res.Path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := install(o); !errors.Is(err, plugininstall.ErrExists) {
		t.Fatalf("second install: err = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(res.Path); string(got) != "old" {
		t.Errorf("the refused install changed the file: %q", got)
	}
	if es := entries(t, dir); len(es) != 1 {
		t.Errorf("plugins dir holds %v", es)
	}
	o.Force = true
	if _, err := install(o); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, payload) {
		t.Errorf("--force did not replace the file: %q", got)
	}
	if es := entries(t, dir); len(es) != 1 {
		t.Errorf("plugins dir holds %v", es)
	}
}

func TestInstallFromLocalPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tobira-driver-local")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := pluginsDir(t)
	res, err := install(plugininstall.Options{Source: src, SHA256: sum(payload), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "local" || res.Resolved != "" {
		t.Errorf("Result = %+v", res)
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, payload) {
		t.Errorf("installed %q", got)
	}

	t.Run("bad checksum", func(t *testing.T) {
		d := pluginsDir(t)
		_, err := install(plugininstall.Options{Source: src, SHA256: sum(nil), Dir: d})
		if !errors.Is(err, plugininstall.ErrChecksum) {
			t.Fatalf("err = %v", err)
		}
		if es := entries(t, d); len(es) != 0 {
			t.Errorf("plugins dir holds %v", es)
		}
	})
	t.Run("directory", func(t *testing.T) {
		d := filepath.Join(t.TempDir(), "tobira-driver-dir")
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := install(plugininstall.Options{Source: d, SHA256: sum(payload), Dir: pluginsDir(t)})
		if !errors.Is(err, plugininstall.ErrInput) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a refused directory", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		_, err := install(plugininstall.Options{Source: src + "-gone", SHA256: sum(payload), Dir: pluginsDir(t)})
		if !errors.Is(err, plugininstall.ErrInput) {
			t.Fatalf("err = %v, want ErrInput", err)
		}
	})
	t.Run("symlink is resolved and reported", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on windows")
		}
		link := filepath.Join(t.TempDir(), "tobira-driver-linked")
		if err := os.Symlink(src, link); err != nil {
			t.Fatal(err)
		}
		res, err := install(plugininstall.Options{Source: link, SHA256: sum(payload), Dir: pluginsDir(t)})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(src)
		if res.Name != "linked" || res.Resolved != want {
			t.Errorf("Result = %+v, want name linked resolved to %s", res, want)
		}
		if st, _ := os.Lstat(res.Path); st.Mode()&os.ModeSymlink != 0 {
			t.Error("installed a symlink, want a copy")
		}
	})
}

func TestInstallFromAPathWithASchemeLikePart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("':' is not allowed in windows file names")
	}
	dir := filepath.Join(t.TempDir(), "weird:", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tobira-driver-local"), payload, 0o755); err != nil {
		t.Fatal(err)
	}
	// filepath.Join would clean "weird://b" to "weird:/b"; keep the "//".
	src := strings.Replace(filepath.Join(dir, "tobira-driver-local"), "weird:/b", "weird://b", 1)
	if !strings.Contains(src, "://") {
		t.Fatalf("source %q has no ://", src)
	}
	res, err := install(plugininstall.Options{Source: src, SHA256: sum(payload), Dir: pluginsDir(t)})
	if err != nil {
		t.Fatalf("a local path containing :// was not installed: %v", err)
	}
	if res.Name != "local" {
		t.Errorf("Result = %+v", res)
	}
}

func TestInstallRefusesAnUnsafeDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are unix semantics")
	}
	srv, _ := serve(t, payload)
	dir := filepath.Join(t.TempDir(), "plugins")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := install(plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: dir})
	if !errors.Is(err, plugininstall.ErrUnsafeDir) {
		t.Fatalf("err = %v, want ErrUnsafeDir", err)
	}
	if es := entries(t, dir); len(es) != 0 {
		t.Errorf("plugins dir holds %v", es)
	}

	group := filepath.Join(t.TempDir(), "plugins")
	if err := os.Mkdir(group, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(group, 0o770); err != nil {
		t.Fatal(err)
	}
	_, err = install(plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: group})
	if !errors.Is(err, plugininstall.ErrUnsafeDir) || !strings.Contains(err.Error(), "group-writable") {
		t.Fatalf("group-writable dir: err = %v, want ErrUnsafeDir", err)
	}

	file := filepath.Join(t.TempDir(), "plugins")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := install(plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: file}); err == nil {
		t.Fatal("installed into a file")
	}
	if _, err := install(plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: "relative/plugins"}); !errors.Is(err, plugininstall.ErrInput) {
		t.Fatalf("relative dir: err = %v, want ErrInput", err)
	}
}

func TestInstallHonoursContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload[:4])
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	dir := pluginsDir(t)
	_, err := plugininstall.Install(ctx, plugininstall.Options{Source: srv.URL + "/tobira-driver-demo", SHA256: sum(payload), Dir: dir})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if es := entries(t, dir); len(es) != 0 {
		t.Errorf("plugins dir holds %v", es)
	}
}
