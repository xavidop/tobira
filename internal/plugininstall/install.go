// Package plugininstall puts a driver plugin binary into the plugins
// directory for `tobira driver install`: from an http(s) URL or a local
// path, only once its sha256 matches, through a temporary file renamed
// into place. It never runs the binary.
package plugininstall

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/xavidop/tobira/plugin/host"
)

// MaxSize is the default cap on a plugin binary: 256 MiB.
const MaxSize int64 = 256 << 20

// maxRedirects is how many redirects a download may follow.
const maxRedirects = 5

// Transport timeouts, which hold even when the caller's ctx has no
// deadline (`--timeout 0`), so a server that never answers cannot hang an
// install forever. The body itself is bounded only by ctx.
const (
	dialTimeout           = 30 * time.Second
	tlsHandshakeTimeout   = 30 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// schemeRE is a URL scheme followed by "://" (RFC 3986: a letter, then
// letters, digits, "+", "-" or "."). It is never mutated.
var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// isURL reports whether source starts with a scheme and "://"; anything
// else, a path containing "://" further on included, is a local path.
func isURL(source string) bool { return schemeRE.MatchString(source) }

// The kinds of Install errors; test them with errors.Is.
var (
	// ErrInput is a bad checksum, source, name or plugins directory.
	ErrInput = errors.New("invalid install request")
	// ErrUnsafeDir is a plugins directory others can write to.
	ErrUnsafeDir = errors.New("unsafe plugins directory")
	// ErrExists is a plugin file already there, without Force.
	ErrExists = errors.New("plugin already installed")
	// ErrDownload is a failed download: the network, a refused
	// redirect or a status other than 200.
	ErrDownload = errors.New("download failed")
	// ErrTooLarge is a binary over the size cap.
	ErrTooLarge = errors.New("plugin binary too large")
	// ErrChecksum is a binary whose sha256 is not the one given.
	ErrChecksum = errors.New("checksum mismatch")
)

// Options describe one install.
type Options struct {
	// Source is an http or https URL, or a local file path.
	Source string
	// SHA256 is the expected checksum: exactly 64 hex characters, in
	// either case.
	SHA256 string
	// Name is the plugin's file name (tobira-driver-<name>) or its driver
	// name; empty means the base name of Source, which must then be a
	// plugin file name.
	Name string
	// Dir is the plugins directory, an absolute path. It is created 0700
	// when missing and must not be world-writable.
	Dir string
	// Force replaces a plugin file already there.
	Force bool
	// MaxBytes caps the binary; 0 means MaxSize.
	MaxBytes int64
	// UserAgent is sent with a download.
	UserAgent string
}

// Result is an installed plugin.
type Result struct {
	// Name is the driver name.
	Name string
	// Path is where the binary was written.
	Path string
	// Resolved is the file a local Source symlink pointed to, which is
	// what was copied; empty when Source was not a symlink.
	Resolved string
	// Bytes is the binary's size.
	Bytes int64
}

// inputError is an ErrInput whose message is only what is wrong.
type inputError struct{ msg string }

func (e *inputError) Error() string        { return e.msg }
func (e *inputError) Is(target error) bool { return target == ErrInput }

func inputErr(format string, args ...any) error {
	return &inputError{fmt.Sprintf(format, args...)}
}

// ParseSHA256 decodes a checksum of exactly 64 hex characters.
func ParseSHA256(s string) ([]byte, error) {
	if s == "" {
		return nil, inputErr("--sha256 is required: give the binary's sha256 as 64 hex characters")
	}
	if len(s) != 64 {
		return nil, inputErr("--sha256 must be 64 hex characters, got %d characters", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, inputErr("--sha256 must be 64 hex characters")
	}
	return b, nil
}

// FileName checks a plugin name, given as a file name (tobira-driver-x)
// or a driver name (x), and returns the file name to write and the driver
// name. Path separators, "..", NUL and anything host.ValidName refuses
// are errors.
func FileName(name string) (file, driverName string, err error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") {
		return "", "", inputErr("plugin name %q must be a plain file name like tobira-driver-<name>", name)
	}
	file = name
	if !strings.HasPrefix(file, host.Prefix) {
		file = host.Prefix + file
	}
	driverName, ok := host.ValidName(file)
	if !ok {
		return "", "", inputErr("plugin name %q must match tobira-driver-[a-z0-9][a-z0-9-]{0,62}", name)
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(file), ".exe") {
		file += ".exe"
	}
	return file, driverName, nil
}

// Install downloads or copies o.Source into o.Dir, checks its sha256 and
// renames it into place with mode 0755. Nothing is left in o.Dir when it
// fails. It never runs the binary.
func Install(ctx context.Context, o Options) (Result, error) {
	want, err := ParseSHA256(o.SHA256)
	if err != nil {
		return Result{}, err
	}
	max := o.MaxBytes
	if max <= 0 {
		max = MaxSize
	}
	var u *url.URL
	base := ""
	if isURL(o.Source) {
		if u, err = url.Parse(o.Source); err != nil {
			return Result{}, inputErr("not a valid URL")
		}
		if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Host == "" {
			return Result{}, inputErr("only http and https URLs (or a local path) can be installed")
		}
		base = path.Base(u.Path)
	} else {
		if o.Source == "" {
			return Result{}, inputErr("no URL or path to install")
		}
		base = filepath.Base(o.Source)
	}
	name := o.Name
	if name == "" {
		name = base
		if !strings.HasPrefix(name, host.Prefix) {
			return Result{}, inputErr("%q is not a plugin file name (tobira-driver-<name>); give --name", name)
		}
	}
	file, driverName, err := FileName(name)
	if err != nil {
		return Result{}, err
	}
	dir, err := prepareDir(o.Dir)
	if err != nil {
		return Result{}, err
	}
	dest := filepath.Join(dir, file)
	if filepath.Dir(dest) != dir {
		return Result{}, inputErr("plugin name %q leaves the plugins directory", name)
	}
	// A quick look to spare a pointless download; the O_EXCL create in
	// place() is what actually refuses an existing file.
	if !o.Force {
		if _, err := os.Lstat(dest); err == nil {
			return Result{}, existsErr(dest)
		}
	}

	res := Result{Name: driverName, Path: dest}
	var src io.ReadCloser
	if u != nil {
		src, err = download(ctx, u, o.UserAgent, max)
	} else {
		src, res.Resolved, err = openLocal(o.Source, max)
	}
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = src.Close() }()

	tmp, err := os.CreateTemp(dir, ".tobira-install-*")
	if err != nil {
		return Result{}, fmt.Errorf("create a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	placed := false
	defer func() {
		if !placed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := copyCapped(ctx, io.MultiWriter(tmp, h), src, max)
	if err != nil {
		return Result{}, err
	}
	res.Bytes = n
	if err := tmp.Sync(); err != nil {
		return Result{}, fmt.Errorf("write the plugin: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fmt.Errorf("write the plugin: %w", err)
	}
	if got := h.Sum(nil); subtle.ConstantTimeCompare(got, want) != 1 {
		return Result{}, fmt.Errorf("%w: the binary's sha256 is %s, not the one given", ErrChecksum, hex.EncodeToString(got))
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return Result{}, fmt.Errorf("make the plugin executable: %w", err)
	}
	if err := place(tmpName, dest, o.Force); err != nil {
		return Result{}, err
	}
	placed = true
	syncDir(dir)
	return res, nil
}

func existsErr(dest string) error {
	return fmt.Errorf("%w: %s exists; give --force to replace it", ErrExists, dest)
}

// prepareDir creates the plugins directory 0700 if needed and checks it.
func prepareDir(dir string) (string, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", inputErr("the plugins directory must be an absolute path (set XDG_CONFIG_HOME or HOME)")
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the plugins directory: %w", err)
	}
	if why := host.UnsafeDir(dir); why != "" {
		return "", fmt.Errorf("%w: %s is %s", ErrUnsafeDir, dir, why)
	}
	return dir, nil
}

// place moves the checked temporary file to dest. Without force it first
// claims dest with O_EXCL, so an existing file (or one created meanwhile)
// is refused, never overwritten.
//
// A known race: a --force install whose rename lands between another
// install's O_EXCL claim and its rename is then overwritten by that
// install. Either way dest ends up as one complete binary whose checksum
// was verified, never a mix, so this is accepted rather than locked.
func place(tmp, dest string, force bool) error {
	if !force {
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return existsErr(dest)
		}
		if err != nil {
			return fmt.Errorf("install the plugin: %w", err)
		}
		_ = f.Close()
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(dest)
			return fmt.Errorf("install the plugin: %w", err)
		}
		return nil
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("install the plugin: %w", err)
	}
	return nil
}

// syncDir makes a rename in dir durable, best effort: a failure (or a
// system where directories cannot be synced) is ignored, since the file
// itself was already synced.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// displayURL is u without credentials or query, for messages.
func displayURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// errRedirect is the reason checkRedirect refused a redirect.
type errRedirect struct{ msg string }

func (e *errRedirect) Error() string { return e.msg }

func checkRedirect(req *http.Request, via []*http.Request) error {
	s := strings.ToLower(req.URL.Scheme)
	if s != "http" && s != "https" {
		return &errRedirect{fmt.Sprintf("refused a redirect to a %q URL: only http and https are followed", req.URL.Scheme)}
	}
	if s == "http" {
		for _, r := range via {
			if strings.EqualFold(r.URL.Scheme, "https") {
				return &errRedirect{"refused a redirect from https to http"}
			}
		}
	}
	if len(via) > maxRedirects {
		return &errRedirect{fmt.Sprintf("stopped after %d redirects", maxRedirects)}
	}
	return nil
}

// newClient is the download client: the redirect policy, proxy settings
// from the environment and the transport timeouts above.
func newClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	tr.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = tlsHandshakeTimeout
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: tr, CheckRedirect: checkRedirect}
}

// download starts a GET of u and returns its body once the server has
// answered 200 with no Content-Length over max.
func download(ctx context.Context, u *url.URL, userAgent string, max int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, inputErr("not a valid URL")
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := newClient().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("download %s: %w", displayURL(u), ctxErr)
		}
		var re *errRedirect
		if errors.As(err, &re) {
			return nil, fmt.Errorf("%w: %s: %s", ErrDownload, displayURL(u), re.msg)
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // its message repeats the full URL
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrDownload, displayURL(u), err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s: the server answered %s", ErrDownload, displayURL(u), resp.Status)
	}
	if resp.ContentLength > max {
		_ = resp.Body.Close()
		return nil, tooLarge(max)
	}
	return resp.Body, nil
}

func tooLarge(max int64) error {
	return fmt.Errorf("%w: it is over the %s limit", ErrTooLarge, size(max))
}

func size(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// openLocal opens a local source file. A symlink is resolved, and the
// file it points to is returned in resolved. Only a regular file is
// accepted, checked before opening (so a FIFO never blocks the open) and
// again on the open file.
func openLocal(p string, max int64) (io.ReadCloser, string, error) {
	lst, err := os.Lstat(p)
	if err != nil {
		return nil, "", inputErr("cannot read %s: %v", p, errText(err))
	}
	target := p
	resolved := ""
	if lst.Mode()&os.ModeSymlink != 0 {
		if target, err = filepath.EvalSymlinks(p); err != nil {
			return nil, "", inputErr("cannot resolve the symlink %s: %v", p, errText(err))
		}
		if target, err = filepath.Abs(target); err != nil {
			return nil, "", inputErr("cannot resolve the symlink %s: %v", p, errText(err))
		}
		resolved = target
	}
	st, err := os.Stat(target)
	if err != nil {
		return nil, "", inputErr("cannot read %s: %v", target, errText(err))
	}
	if !st.Mode().IsRegular() {
		return nil, "", inputErr("%s is not a regular file", target)
	}
	if st.Size() > max {
		return nil, "", tooLarge(max)
	}
	f, err := os.Open(target)
	if err != nil {
		return nil, "", inputErr("cannot read %s: %v", target, errText(err))
	}
	fst, err := f.Stat()
	if err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		_ = f.Close()
		return nil, "", inputErr("%s changed while it was opened", target)
	}
	return f, resolved, nil
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// copyCapped copies src to dst, failing with ErrTooLarge as soon as more
// than max bytes arrive, and with ctx's error once it ends.
func copyCapped(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(ctxReader{ctx, src}, max+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return n, fmt.Errorf("read the plugin: %w", ctxErr)
		}
		return n, fmt.Errorf("%w: copying the plugin: %v", ErrDownload, err)
	}
	if n > max {
		return n, tooLarge(max)
	}
	return n, nil
}

// ctxReader fails reads once ctx ends, for sources (local files) that do
// not watch it themselves.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
