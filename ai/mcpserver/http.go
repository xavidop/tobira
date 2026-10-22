package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/mcphttp"
)

// HTTP configures ServeHTTP.
type HTTP struct {
	// Addr is host:port to listen on; an empty host is 127.0.0.1.
	Addr string
	// InsecureBind allows a non-loopback Addr.
	InsecureBind bool
	// AllowedHosts are extra Host header values to accept
	// (mcphttp.ParseAllowedHost).
	AllowedHosts []string
	// Token is the bearer token (mcp.token, resolved). Zero means one is
	// generated and printed once to Stderr.
	Token driver.Secret
	// Stderr gets the address served and a generated token; nil discards.
	Stderr io.Writer
}

// HTTP server limits.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownGrace     = 5 * time.Second
	shutdownDrain     = 5 * time.Second
	endpointPath      = "/mcp"
)

// ServeHTTP serves the MCP protocol over Streamable HTTP at /mcp on h.Addr
// until ctx is cancelled (nil, after a graceful shutdown of at most five
// seconds). A non-loopback address without h.InsecureBind is refused with
// mcphttp.ErrInsecureBind before anything listens; other errors are a bad
// address, the setup or the listener failing.
//
// Every request passes mcphttp.Guard: its Host must be this server's, any
// Origin must be loopback, and it must carry the bearer token.
func ServeHTTP(ctx context.Context, o Options, h HTTP) error {
	b, err := mcphttp.ParseBind(h.Addr)
	if err != nil {
		return err
	}
	if err := mcphttp.CheckBind(b, h.InsecureBind); err != nil {
		return err
	}
	s, err := newServer(ctx, o)
	if err != nil {
		return err
	}
	return s.listenHTTP(ctx, b, h)
}

// listenHTTP is ServeHTTP once the address is checked and the server built.
func (s *srv) listenHTTP(ctx context.Context, b mcphttp.Bind, h HTTP) error {
	stderr := h.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", b.ListenAddress())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if !h.InsecureBind && !mcphttp.LoopbackAddr(ln.Addr()) {
		_ = ln.Close() // what was bound is not loopback after all
		return mcphttp.ErrInsecureBind
	}
	if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		b.Port = port // the real one when 0 was asked for
	}
	tok := h.Token.Reveal()
	if tok == "" {
		tok = newToken()
	}

	// GET streams last as long as the client keeps them; they end when
	// shutdown starts, so the grace period goes to calls in flight.
	streams, endStreams := context.WithCancel(context.Background())
	defer endStreams()
	s.sessions = newSessionTable(s.now, s.expireSession)
	stopSweep := s.sweepSessions(streams)
	defer stopSweep()
	streamable := server.NewStreamableHTTPServer(s.mcp,
		server.WithSessionIdManager(s.sessions), // only live IDs this server issued
		server.WithLogger(mcpLogger{s.log}),
	)
	mux := http.NewServeMux()
	mux.Handle(endpointPath, s.endSessions(endOnShutdown(streams, streamable)))
	var running inFlight
	srv := &http.Server{
		Handler: running.track(mcphttp.Guard(mux, mcphttp.GuardConfig{
			Bind: b, Listen: ln.Addr().String(), AllowedHosts: h.AllowedHosts, Token: []byte(tok),
		})),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout, // no ReadTimeout or WriteTimeout: responses may stream
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	// A connection that has not sent a request yet carries nothing in
	// flight, but Shutdown would wait up to five seconds for it: close it.
	fresh := &freshConns{}
	srv.ConnState = fresh.track
	srv.RegisterOnShutdown(endStreams)
	srv.RegisterOnShutdown(fresh.closeAll)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	_, _ = fmt.Fprintf(stderr, "MCP HTTP server listening on http://%s%s\n", ln.Addr(), endpointPath)
	if h.Token.IsZero() {
		_, _ = fmt.Fprintf(stderr, "MCP HTTP token: %s (pass as Authorization: Bearer …)\n", tok)
	}
	s.log.Debug("mcp http serving", "addr", ln.Addr().String(), "allowed_hosts", len(h.AllowedHosts))

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		s.log.Debug("mcp http shutdown did not finish in time; closing", "error", err.Error())
		_ = srv.Close()
		// Close does not wait for handlers. The caller closes history and
		// the connections once this returns, so give the calls still
		// running a bounded time to finish and record.
		if !running.wait(s.drain) {
			s.log.Debug("mcp http calls still running after the drain")
		}
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Debug("mcp http serve ended", "error", err.Error())
	}
	return nil
}

// newToken is 32 random bytes in hex.
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

// freshConns tracks the connections that have not sent a request yet.
type freshConns struct{ m sync.Map }

func (f *freshConns) track(c net.Conn, st http.ConnState) {
	if st == http.StateNew {
		f.m.Store(c, struct{}{})
	} else {
		f.m.Delete(c)
	}
}

func (f *freshConns) closeAll() {
	f.m.Range(func(c, _ any) bool {
		_ = c.(net.Conn).Close()
		return true
	})
}

// endOnShutdown ends GET streams when streams is cancelled.
func endOnShutdown(streams context.Context, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(streams, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// endSessions unregisters a session its client ended (DELETE) from the MCP
// server, which mcp-go's handler does not do, so the session's client name
// is dropped (forgetClient). Only a DELETE mcp-go answered 200 ends one.
func (s *srv) endSessions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			next.ServeHTTP(w, r)
			return
		}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if id := r.Header.Get(server.HeaderKeySessionID); id != "" && sw.code == http.StatusOK {
			s.mcp.UnregisterSession(r.Context(), id)
		}
	})
}

// statusWriter records the status a handler answered (200 when it wrote
// none).
type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.code, w.written = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// expireSession drops what the server keeps for a session the table
// expired: its mcp-go registration and its client name.
func (s *srv) expireSession(id string) {
	s.mcp.UnregisterSession(context.Background(), id)
	s.clients.Delete(id)
	s.log.Debug("mcp http session expired after idling")
}

// sessionSweep is how often idle sessions are looked for between requests.
const sessionSweep = time.Minute

// sweepSessions expires idle sessions every minute until ctx ends or the
// returned stop is called.
func (s *srv) sweepSessions(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	tick := time.NewTicker(sessionSweep)
	go func() {
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				s.sessions.sweep()
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel
}

// inFlight counts the handlers running.
type inFlight struct{ n atomic.Int64 }

func (f *inFlight) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.n.Add(1)
		defer f.n.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// wait waits up to d for every handler to return, reporting whether they
// did.
func (f *inFlight) wait(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for f.n.Load() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// mcpLogger sends mcp-go's transport logs to the debug log.
type mcpLogger struct{ log *slog.Logger }

func (l mcpLogger) Infof(format string, v ...any) {
	l.log.Debug("mcp-go: " + fmt.Sprintf(format, v...))
}
func (l mcpLogger) Errorf(format string, v ...any) {
	l.log.Debug("mcp-go error: " + fmt.Sprintf(format, v...))
}
