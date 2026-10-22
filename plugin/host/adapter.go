package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/plugin/convert"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

const (
	// maxListed caps the schemes and export formats kept from a plugin.
	maxListed = 64
	// closeTimeout bounds the calls that take no context (Conn.Close,
	// Invalidate).
	closeTimeout = 5 * time.Second
)

// knownCaps are the capability names the host understands; a plugin's
// other strings are ignored.
var knownCaps = []driver.Capability{
	driver.CapBrowser, driver.CapMutator, driver.CapExplainer, driver.CapTransactor,
	driver.CapVectorSearcher, driver.CapTimeSeriesReader, driver.CapGraphReader,
	driver.CapCompleter, driver.CapExporter, driver.CapWatcher, driver.CapReadOnly,
}

// knownKinds are the driver.Kind values; any other a plugin reports is
// shown as KindOther.
var knownKinds = []driver.Kind{
	driver.KindSQL, driver.KindDocument, driver.KindKeyValue, driver.KindWideColumn, driver.KindVector,
	driver.KindTimeSeries, driver.KindGraph, driver.KindSearch, driver.KindOther,
}

// adapter is the driver.Driver registered for one plugin.
type adapter struct {
	h      *Host
	plugin Plugin
	stub   driver.Info // what Info says until the plugin answers
	routed bool        // the registry routes the plugin's name as a scheme
	// registered turns lazy Info on once Registry.Add has read the stub.
	registered atomic.Bool
	stat       os.FileInfo    // the binary as discovered, re-checked at launch
	starts     sync.WaitGroup // running starts, for Host.Close

	mu       sync.Mutex
	cur      *proc
	starting *start
	info     *driver.Info
	closed   bool
}

// start is one start of the plugin process, shared by every caller that
// needs the process while it runs.
type start struct {
	done chan struct{}
	p    *proc
	err  error
}

func (a *adapter) errClosed() error {
	return driver.Wrap(driver.ErrConnection, a.plugin.Name, "start", errors.New("the plugin host is closed"))
}

// process returns a live process, starting one when there is none (or the
// last one died). A start runs on its own, bounded by the StartTimeout:
// a caller whose ctx ends stops waiting, and the next caller reuses it.
func (a *adapter) process(ctx context.Context) (*proc, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, a.errClosed()
	}
	if a.cur != nil && a.cur.alive() {
		p := a.cur
		a.mu.Unlock()
		return p, nil
	}
	if a.starting == nil {
		old := a.cur
		a.cur = nil
		s := &start{done: make(chan struct{})}
		a.starting = s
		a.starts.Add(1)
		go a.run(s, old)
	}
	s := a.starting
	a.mu.Unlock()
	select {
	case <-s.done:
		return s.p, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run performs start s, first reaping old, the dead previous process.
func (a *adapter) run(s *start, old *proc) {
	defer a.starts.Done()
	if old != nil {
		a.h.log.Debug("plugin exited; restarting", "plugin", a.plugin.Name)
		old.kill()
	}
	p, err := a.launch(a.h.lifetime)
	a.mu.Lock()
	a.starting = nil
	if err == nil && a.closed {
		err = a.errClosed()
	}
	if err == nil {
		a.cur = p
	}
	s.p, s.err = p, err
	close(s.done)
	a.mu.Unlock()
	if err != nil {
		a.h.log.Debug("plugin start failed", "plugin", a.plugin.Name, "err", err.Error())
		if p != nil {
			p.kill()
		}
	}
}

// close stops the current process and refuses new ones.
func (a *adapter) close() {
	a.mu.Lock()
	a.closed = true
	p := a.cur
	a.cur = nil
	a.mu.Unlock()
	if p != nil {
		p.kill()
	}
}

// Info implements driver.Driver without blocking and without starting
// anything: it returns the plugin's Info once the plugin has answered (on
// an Open, or through Host.Info), and the stub until then (always the stub
// before Register finishes, since the registry indexes that).
func (a *adapter) Info() driver.Info {
	info, _ := a.cachedInfo()
	return info
}

// cachedInfo is Info, also reporting whether it is the plugin's answer.
func (a *adapter) cachedInfo() (driver.Info, bool) {
	if !a.registered.Load() {
		return a.stub, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.info != nil {
		return cloneInfo(*a.info), true
	}
	return cloneInfo(a.stub), false
}

// loadInfo returns the cached Info or asks the plugin for it.
func (a *adapter) loadInfo(ctx context.Context) (driver.Info, error) {
	a.mu.Lock()
	if a.info != nil {
		info := cloneInfo(*a.info)
		a.mu.Unlock()
		return info, nil
	}
	a.mu.Unlock()
	info, err := a.fetchInfo(ctx)
	if err != nil {
		return driver.Info{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.info = &info
	return cloneInfo(info), nil
}

func (a *adapter) fetchInfo(ctx context.Context) (driver.Info, error) {
	p, err := a.process(ctx)
	if err != nil {
		return driver.Info{}, err
	}
	resp, err := p.rpc.Info(ctx, &pb.InfoRequest{})
	if err != nil {
		return driver.Info{}, a.callError(ctx, p, "info", err, driver.ConnConfig{})
	}
	info, err := convert.InfoFromProto(resp.GetInfo())
	if err != nil {
		return driver.Info{}, fmt.Errorf("plugin %q: info: %w", a.plugin.Name, err)
	}
	return a.normalize(info), nil
}

// normalize makes a plugin's Info safe to show and consistent with the
// registry: the registry name wins, unknown capabilities are dropped, and
// the routed scheme is listed first.
func (a *adapter) normalize(info driver.Info) driver.Info {
	if info.Name != a.plugin.Name {
		a.h.log.Debug("plugin reports another name; the file name wins", "plugin", a.plugin.Name)
	}
	info.Name = a.plugin.Name
	if !slices.Contains(knownKinds, info.Kind) {
		info.Kind = driver.KindOther
	}
	info.Capabilities = filterCaps(info.Capabilities)
	var schemes []string
	if a.routed {
		schemes = append(schemes, a.plugin.Name)
	}
	for _, s := range info.Schemes {
		s = strings.ToLower(s) // as Registry.Add does
		if s != "" && !slices.Contains(schemes, s) && len(schemes) < maxListed {
			schemes = append(schemes, s)
		}
	}
	info.Schemes = schemes
	return info
}

func cloneInfo(i driver.Info) driver.Info {
	i.Schemes = slices.Clone(i.Schemes)
	i.Capabilities = slices.Clone(i.Capabilities)
	return i
}

// filterCaps keeps the known capability names, once each, in order.
func filterCaps(in []driver.Capability) []driver.Capability {
	var out []driver.Capability
	for _, c := range in {
		if slices.Contains(knownCaps, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// capsFromStrings decodes a plugin's capability list, keeping known names
// only, once each.
func capsFromStrings(in []string) []driver.Capability {
	var out []driver.Capability
	for _, s := range in {
		c := driver.Capability(s)
		if slices.Contains(knownCaps, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// Open implements driver.Driver. It starts the plugin if needed. When the
// process turns out to be dead (the transport fails), it is replaced and
// the Open tried once more.
func (a *adapter) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	conn, retry, err := a.open(ctx, cfg)
	if retry && ctx.Err() == nil {
		conn, _, err = a.open(ctx, cfg)
	}
	return conn, err
}

func (a *adapter) open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, bool, error) {
	p, err := a.process(ctx)
	if err != nil {
		return nil, false, err
	}
	p.redact.add(cfg)
	resp, err := p.rpc.Open(ctx, &pb.OpenRequest{Config: convert.ConnConfigToProto(cfg)})
	if err != nil {
		err = a.callError(ctx, p, "open", err, cfg)
		return nil, !p.alive(), err
	}
	h := resp.GetConn()
	if h == nil || h.GetId() == "" || len(h.GetId()) > convert.MaxNameBytes {
		return nil, false, fmt.Errorf("plugin %q: open: %w: bad connection handle", a.plugin.Name, convert.ErrInvalid)
	}
	handle := &pb.ConnHandle{Id: h.GetId()}
	caps, err := a.allowed(ctx, capsFromStrings(resp.GetCapabilities()))
	if err != nil {
		// No Conn without the plugin's Info: release the plugin's side.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		_, _ = p.rpc.Close(cctx, &pb.CloseRequest{Conn: handle})
		cancel()
		return nil, false, err
	}
	c := &conn{a: a, p: p, h: handle, cfg: cfg, caps: caps}
	if slices.Contains(c.caps, driver.CapExporter) {
		c.formats = boundedNames(resp.GetExportFormats())
	}
	if resp.GetCatalogInvalidator() {
		return &invalidatorConn{c}, false, nil
	}
	return c, false, nil
}

// allowed keeps the capabilities the plugin also declared in its Info, so
// Open cannot unlock an RPC Info never advertised. The process is running,
// so loading Info here is one RPC at most (no lock is held). When Info
// fails or ctx ends first, it returns the error: Open then fails rather
// than return a Conn with less than the plugin offers.
func (a *adapter) allowed(ctx context.Context, caps []driver.Capability) ([]driver.Capability, error) {
	info, err := a.loadInfo(ctx)
	if err != nil {
		return nil, err
	}
	return intersect(caps, info.Capabilities), nil
}

// intersect keeps the entries of caps that are also in of, in caps' order.
func intersect(caps, of []driver.Capability) []driver.Capability {
	var out []driver.Capability
	for _, c := range caps {
		if slices.Contains(of, c) {
			out = append(out, c)
		}
	}
	return out
}

// boundedNames keeps at most maxListed non-empty names within the name cap.
func boundedNames(in []string) []string {
	var out []string
	for _, s := range in {
		if len(out) == maxListed {
			break
		}
		if s != "" && len(s) <= convert.MaxNameBytes {
			out = append(out, s)
		}
	}
	return out
}

// callError turns a failed RPC into the driver error the host returns.
// A transport failure while ctx is live (the process died, or its
// connection closed) is driver.ErrConnection and marks the process broken;
// a cancellation the caller asked for stays a context error. Every message
// is redacted again with cfg: a plugin is not trusted to have done it. A
// plugin's driver error is attributed to the registered name.
func (a *adapter) callError(ctx context.Context, p *proc, op string, err error, cfg driver.ConnConfig) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return redactErr(err, cfg)
	}
	info := convert.ErrorInfoOf(st)
	if info == nil && ctx.Err() == nil && (st.Code() == codes.Unavailable || st.Code() == codes.Canceled) && !p.healthy() {
		p.broken.Store(true)
		return a.lost(op)
	}
	if info != nil {
		// Cut hostile sizes first, with slack so a secret straddling the
		// cut is still whole when it is masked; FromErrorInfo then caps.
		red := func(s string) string {
			if len(s) > convert.MaxNameBytes+stderrSlack {
				s = s[:convert.MaxNameBytes+stderrSlack]
			}
			return driver.RedactMessage(s, cfg)
		}
		info = &pb.ErrorInfo{
			Kind: info.GetKind(), Code: red(info.GetCode()), Message: red(info.GetMessage()),
			// The error is the registered driver's, whatever name the
			// plugin gives itself (the registry name wins, as in Info).
			Driver: a.plugin.Name, Op: red(info.GetOp()),
		}
		var cause error
		switch st.Code() {
		case codes.Canceled:
			cause = context.Canceled
		case codes.DeadlineExceeded:
			cause = context.DeadlineExceeded
		}
		return convert.FromErrorInfo(info, cause)
	}
	return convert.FromStatus(status.New(st.Code(), driver.RedactMessage(st.Message(), cfg)).Err())
}

// lost is the error for a call on a process that is gone.
func (a *adapter) lost(op string) error {
	return driver.Wrap(driver.ErrConnection, a.plugin.Name, op, errors.New("the plugin process exited"))
}

// redactErr masks cfg's credentials in an error that is not a status (a
// decode failure, say), keeping what errors.Is sees.
func redactErr(err error, cfg driver.ConnConfig) error {
	msg := err.Error()
	if red := driver.RedactMessage(msg, cfg); red != msg {
		return &redactedError{msg: red, err: err}
	}
	return err
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
