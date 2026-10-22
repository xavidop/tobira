// Package connform is the add/edit connection form: a charm.land/huh/v2
// form wrapped as a component the TUI embeds as an overlay and the CLI runs
// standalone. It never accepts a literal password, only a secret ref, and it
// never writes anything itself: it emits SavedMsg and the caller persists.
package connform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// Mode is whether the form adds a connection or edits one.
type Mode int

const (
	// Add creates a new connection.
	Add Mode = iota
	// Edit changes an existing connection, which may be renamed.
	Edit
)

// Tester checks that c can connect: resolve its password, open, ping, close.
// Its errors must already be redacted by the caller; the form masks DSN
// credentials again and escapes control characters before showing them.
type Tester func(ctx context.Context, c config.Connection) error

// SavedMsg is emitted when the user submits a valid form. OldName is the
// name the connection had when the form opened, "" in Add mode.
type SavedMsg struct {
	Conn    config.Connection
	OldName string
}

// CancelledMsg is emitted when the user dismisses the form.
type CancelledMsg struct{}

// ErrCancelled is what Run returns when the user dismisses the form.
var ErrCancelled = errors.New("cancelled")

// testDoneMsg is the result of a Test connection action.
type testDoneMsg struct {
	id   uint64
	err  error
	took time.Duration
}

// formChrome is the number of lines below the fields: status and hint.
const formChrome = 3

// footerRoom is the lines kept below the fields for huh's validation errors,
// which it draws under the scrolling field area.
const footerRoom = 4

// fieldsHeight is the height given to the huh form for a total height h.
func fieldsHeight(h int) int { return max(h-formChrome-footerRoom, 1) }

// values holds what the fields are bound to. It is shared by every copy of a
// Model and by every form built from it, so a rebuilt form shows what the
// user had typed.
type values struct {
	name, driver, dsn, password, color, tags string
	readOnly                                 bool
	options                                  map[string]string // carried over, not editable
	taken                                    map[string]bool
	own                                      string // the name being edited, allowed to stay
	origColor                                string // the colour the connection had, kept even if unlisted
	origPassword                             string // the password ref it had, kept even if only a loader accepts it
	dsnRedacted                              bool   // the DSN shown is the redacted form of one with a password
}

// dsnHint is shown when a hand-written DSN password was masked in the form.
const dsnHint = "the DSN contained a password; move it into a password ref"

// state is the mutable, non-field state shared by copies of a Model.
type state struct {
	testID  uint64
	running bool
	cancel  context.CancelFunc
	status  string
	isErr   bool
}

// Model is the form component. Build one with New; the zero value is not
// usable. Like the other TUI components it is a value type whose mutable
// parts are shared pointers.
type Model struct {
	mode    Mode
	oldName string
	drivers []driver.Info
	th      theme.Theme
	tester  Tester
	v       *values
	s       *state
	form    *huh.Form
	w, h    int
}

// New builds a form. existing pre-fills Edit mode (and is ignored in Add
// mode). drivers are the registered drivers the Driver field offers; tester
// may be nil, in which case Test connection is unavailable.
func New(mode Mode, existing *config.Connection, drivers []driver.Info, th theme.Theme, tester Tester) Model {
	v := &values{taken: map[string]bool{}}
	m := Model{mode: mode, drivers: drivers, th: th, tester: tester, v: v, s: &state{}, w: 80, h: 30}
	if mode == Edit && existing != nil {
		m.oldName = existing.Name
		v.own = existing.Name
		v.name, v.driver, v.dsn = existing.Name, existing.Driver, existing.DSN
		v.password, v.color, v.readOnly = existing.Password, existing.Color, existing.ReadOnly
		v.origColor = existing.Color
		v.origPassword = existing.Password
		// Never show a password written into the DSN by hand: pre-fill the
		// masked form and say what to do.
		if red := driver.RedactDSN(existing.DSN); red != existing.DSN {
			v.dsn, v.dsnRedacted = red, true
		}
		v.tags = strings.Join(existing.Tags, ", ")
		v.options = existing.Options
		if v.driver == "" {
			v.driver = driverForScheme(drivers, existing.DSN)
		}
	} else if len(drivers) > 0 {
		v.driver = drivers[0].Name
	}
	m.form = m.build()
	return m
}

func driverForScheme(drivers []driver.Info, dsn string) string {
	sc := driver.Scheme(dsn)
	for _, d := range drivers {
		for _, s := range d.Schemes {
			if s == sc {
				return d.Name
			}
		}
	}
	return ""
}

// WithTaken sets the connection names already in use, which the Name field
// rejects (the name being edited stays allowed).
func (m Model) WithTaken(names ...string) Model {
	m.v.taken = map[string]bool{}
	for _, n := range names {
		m.v.taken[n] = true
	}
	return m
}

func (m Model) build() *huh.Form {
	v := m.v
	drivers := m.drivers

	dopts := make([]huh.Option[string], 0, len(drivers)+1)
	known := false
	for _, d := range drivers {
		known = known || d.Name == v.driver
		dopts = append(dopts, huh.NewOption(fmt.Sprintf("%s (%s)", d.Name, d.Kind), d.Name))
	}
	if !known && v.driver != "" {
		dopts = append(dopts, huh.NewOption(query.EscapeControls(v.driver)+" (not installed)", v.driver))
	}
	copts := []huh.Option[string]{huh.NewOption("(none)", "")}
	knownColor := v.color == ""
	for _, c := range config.ColorNames() {
		knownColor = knownColor || c == v.color
		copts = append(copts, huh.NewOption(c, c))
	}
	if !knownColor {
		// Keep a colour the list does not know instead of resetting it.
		copts = append(copts, huh.NewOption(query.EscapeControls(v.color), v.color))
	}

	fields := []huh.Field{
		huh.NewInput().Key("name").Title("Name").Description("letters, digits, . _ -").
			Value(&v.name).Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if err := config.ValidateName(s); err != nil {
				return err
			}
			if v.taken[s] && s != v.own {
				return fmt.Errorf("connection %q already exists", driver.RedactMessage(s, driver.ConnConfig{}))
			}
			return nil
		}),
		huh.NewSelect[string]().Key("driver").Title("Driver").Options(dopts...).
			Inline(true).Filtering(false).Value(&v.driver),
		huh.NewInput().Key("dsn").Title("DSN").DescriptionFunc(func() string {
			if v.dsnRedacted && driver.HasPassword(strings.TrimSpace(v.dsn)) {
				return dsnHint
			}
			return "no password here; use the password ref below"
		}, &v.dsn).
			Value(&v.dsn).Validate(func(s string) error {
			s = strings.TrimSpace(s)
			if v.dsnRedacted && driver.HasPassword(s) {
				return errors.New(dsnHint)
			}
			return config.ValidateDSN(s, v.driver, drivers)
		}),
		huh.NewInput().Key("password").Title("Password ref (optional)").
			Description("env:NAME, file:///path, dotenv://path#KEY or keyring:NAME (tobira connections set-password stores one); TOBIRA_CONN_<NAME>_PASSWORD also works").
			Value(&v.password).Validate(func(s string) error {
			return checkRef(strings.TrimSpace(s), v.origPassword)
		}),
		huh.NewConfirm().Key("read_only").Title("Read-only").Affirmative("Yes").Negative("No").Value(&v.readOnly),
		huh.NewSelect[string]().Key("color").Title("Color").Options(copts...).
			Inline(true).Filtering(false).Value(&v.color),
		huh.NewInput().Key("tags").Title("Tags (comma-separated)").Value(&v.tags),
	}
	f := huh.NewForm(huh.NewGroup(fields...)).
		WithTheme(huhTheme(m.th)).
		WithShowHelp(false).
		WithShowErrors(true).
		WithWidth(max(m.w, 1)).
		WithHeight(fieldsHeight(m.h))
	return f
}

// huhTheme paints the form with the tobira palette.
func huhTheme(th theme.Theme) huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		s := huh.ThemeBase(true)
		s.FieldSeparator = lipgloss.NewStyle().SetString("\n")
		fg := lipgloss.NewStyle().Foreground(th.Fg)
		for i, f := range []*huh.FieldStyles{&s.Focused, &s.Blurred} {
			focused := i == 0
			title, border := th.Muted, th.Muted
			if focused {
				title, border = th.Accent, th.Accent
			}
			f.Base = f.Base.BorderForeground(border)
			f.Card = f.Base
			f.Title = lipgloss.NewStyle().Foreground(title).Bold(focused)
			f.NoteTitle = f.Title
			f.Description = lipgloss.NewStyle().Foreground(th.Muted)
			f.ErrorIndicator = lipgloss.NewStyle().SetString(" *").Foreground(th.Danger)
			f.ErrorMessage = lipgloss.NewStyle().SetString(" *").Foreground(th.Danger)
			f.SelectSelector = lipgloss.NewStyle().SetString("> ").Foreground(th.Accent)
			f.NextIndicator = f.NextIndicator.Foreground(th.Muted)
			f.PrevIndicator = f.PrevIndicator.Foreground(th.Muted)
			f.Option = fg
			f.SelectedOption = lipgloss.NewStyle().Foreground(th.Accent)
			f.FocusedButton = f.FocusedButton.Foreground(th.Bg).Background(th.Accent)
			f.BlurredButton = f.BlurredButton.Foreground(th.Muted).Background(th.Bg)
			f.TextInput.Cursor = lipgloss.NewStyle().Foreground(th.Accent)
			f.TextInput.Placeholder = lipgloss.NewStyle().Foreground(th.Muted)
			f.TextInput.Prompt = lipgloss.NewStyle().Foreground(th.Accent)
			f.TextInput.Text = fg
		}
		return s
	})
}

// Title is the heading for the form's pane.
func (m Model) Title() string {
	if m.mode == Edit {
		return "edit connection"
	}
	return "add connection"
}

// Init starts the form (focuses its first field).
func (m Model) Init() tea.Cmd { return m.form.Init() }

// Resize sets the area the form is drawn in.
func (m Model) Resize(w, h int) Model {
	if w == m.w && h == m.h {
		return m
	}
	m.w, m.h = w, h
	m.form.WithWidth(max(w, 1)).WithHeight(fieldsHeight(h))
	return m
}

// Testing reports whether a Test connection is running.
func (m Model) Testing() bool { return m.s.running }

// CancelTest cancels a running Test connection and reports whether there was
// one. Its late result is dropped.
func (m Model) CancelTest() bool {
	if !m.s.running {
		return false
	}
	m.stopTest()
	m.s.status, m.s.isErr = "test cancelled", false
	return true
}

func (m Model) stopTest() {
	if m.s.cancel != nil {
		m.s.cancel()
		m.s.cancel = nil
	}
	m.s.running = false
	m.s.testID++ // whatever is in flight is now stale
}

// SetError rebuilds the form with its current values and shows text under
// it; the caller should run Init on the result. Used for failures only the
// caller can find, such as a duplicate name or a changed file.
func (m Model) SetError(text string) Model {
	m.s.status, m.s.isErr = text, true
	m.form = m.build()
	return m
}

func (m Model) set(text string, isErr bool) Model {
	m.s.status, m.s.isErr = text, isErr
	return m
}

// connection assembles the Connection the fields describe.
func (m Model) connection() config.Connection {
	v := m.v
	c := config.Connection{
		Name:     strings.TrimSpace(v.name),
		Driver:   v.driver,
		DSN:      strings.TrimSpace(v.dsn),
		Password: strings.TrimSpace(v.password),
		Color:    v.color,
		ReadOnly: v.readOnly,
		Options:  v.options,
	}
	for _, t := range strings.Split(v.tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.Tags = append(c.Tags, t)
		}
	}
	return c
}

// Update handles keys and test results. esc and ctrl+c dismiss the form
// (ctrl+c first cancels a running test); ctrl+s saves from any field and
// ctrl+t tests the connection.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case testDoneMsg:
		return m.testDone(msg), nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			return m.dismiss()
		case "ctrl+c":
			if m.CancelTest() {
				return m, nil
			}
			return m.dismiss()
		case "ctrl+t":
			return m.startTest()
		case "ctrl+s":
			return m.submit()
		}
	}
	_, cmd := m.form.Update(msg)
	if m.form.State == huh.StateCompleted {
		return m.submit()
	}
	return m, cmd
}

func (m Model) dismiss() (Model, tea.Cmd) {
	m.stopTest()
	return m, func() tea.Msg { return CancelledMsg{} }
}

// checkRef validates a password ref. An unchanged ref (orig) is held to the
// loader's rule, which also accepts URL-form refs of providers that are not
// compiled in; a changed one must be a registered scheme.
func checkRef(ref, orig string) error {
	if ref != "" && ref == orig {
		return config.ValidateLoadedPasswordRef(ref)
	}
	return config.ValidatePasswordRef(ref)
}

// validate checks c; an unchanged colour or password ref is accepted even
// when the strict check would refuse it, so editing a hand-written
// connection does not force a new one.
func (m Model) validate(c config.Connection) error {
	probe := c
	if probe.Color != "" && probe.Color == m.v.origColor {
		probe.Color = ""
	}
	if err := checkRef(probe.Password, m.v.origPassword); err != nil {
		return err
	}
	probe.Password = ""
	if m.v.dsnRedacted && driver.HasPassword(probe.DSN) {
		return errors.New(dsnHint)
	}
	return config.ValidateConnection(probe, m.drivers)
}

func (m Model) submit() (Model, tea.Cmd) {
	c := m.connection()
	if err := m.validate(c); err != nil {
		m = m.SetError(err.Error())
		return m, m.Init()
	}
	if m.v.taken[c.Name] && c.Name != m.v.own {
		m = m.SetError(fmt.Sprintf("connection %q already exists", driver.RedactMessage(c.Name, driver.ConnConfig{})))
		return m, m.Init()
	}
	m.stopTest()
	m.s.status = ""
	saved := SavedMsg{Conn: c, OldName: m.oldName}
	return m, func() tea.Msg { return saved }
}

func (m Model) startTest() (Model, tea.Cmd) {
	if m.tester == nil {
		return m.set("testing is not available", true), nil
	}
	// The probe always has a name, so the secret resolver never looks up
	// the TOBIRA_CONN__PASSWORD variable of an empty one.
	c := m.connection()
	if c.Name == "" {
		c.Name = "test"
	}
	if err := m.validate(c); err != nil {
		return m.set(err.Error(), true), nil
	}
	m.stopTest()
	ctx, cancel := context.WithCancel(context.Background())
	m.s.cancel, m.s.running = cancel, true
	id := m.s.testID
	m.s.status, m.s.isErr = "testing... (ctrl+c cancels)", false
	tester := m.tester
	return m, func() tea.Msg {
		start := time.Now()
		err := tester(ctx, c)
		return testDoneMsg{id: id, err: err, took: time.Since(start)}
	}
}

func (m Model) testDone(msg testDoneMsg) Model {
	if !m.s.running || msg.id != m.s.testID {
		return m // cancelled or superseded
	}
	m.s.running = false
	if m.s.cancel != nil {
		m.s.cancel()
		m.s.cancel = nil
	}
	if msg.err != nil {
		text := driver.RedactMessage(msg.err.Error(), driver.ConnConfig{DSN: m.v.dsn})
		return m.set("failed: "+text, true)
	}
	return m.set(fmt.Sprintf("ok (%dms)", msg.took.Milliseconds()), false)
}

// View draws the form, then a status line and a hint line.
func (m Model) View() string {
	lines := strings.Split(strings.TrimRight(m.form.View(), "\n"), "\n")
	body := max(m.h-formChrome, 0)
	if len(lines) > body {
		lines = lines[:body]
	}
	for len(lines) < body {
		lines = append(lines, "")
	}
	status := ""
	if m.s.status != "" {
		st := lipgloss.NewStyle().Foreground(m.th.OK)
		if m.s.isErr {
			st = lipgloss.NewStyle().Foreground(m.th.Danger)
		}
		status = st.Render(query.Truncate(query.EscapeControls(m.s.status), max(m.w, 1)))
	}
	hint := lipgloss.NewStyle().Foreground(m.th.Muted).
		Render(query.Truncate("enter next · ctrl+s save · ctrl+t test · esc cancel", max(m.w, 1)))
	lines = append(lines, "", status, hint)
	for i, l := range lines {
		if ansi.StringWidth(l) > m.w && m.w > 0 {
			lines[i] = ansi.Truncate(l, m.w, "")
		}
	}
	return strings.Join(lines, "\n")
}

// Run shows the form on in and out until it is saved or dismissed. It
// returns the connection, or ErrCancelled.
func Run(ctx context.Context, in io.Reader, out io.Writer, m Model) (config.Connection, error) {
	r := &runner{m: m}
	p := tea.NewProgram(r, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	if _, err := p.Run(); err != nil {
		if ctx.Err() != nil {
			return config.Connection{}, ctx.Err()
		}
		return config.Connection{}, err
	}
	if r.cancelled || !r.saved {
		return config.Connection{}, ErrCancelled
	}
	return r.conn, nil
}

type runner struct {
	m         Model
	conn      config.Connection
	saved     bool
	cancelled bool
}

func (r *runner) Init() tea.Cmd { return r.m.Init() }

func (r *runner) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		r.m = r.m.Resize(msg.Width, msg.Height)
		return r, nil
	case SavedMsg:
		r.conn, r.saved = msg.Conn, true
		return r, tea.Quit
	case CancelledMsg:
		r.cancelled = true
		return r, tea.Quit
	}
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	return r, cmd
}

func (r *runner) View() tea.View { return tea.NewView(r.m.View()) }
