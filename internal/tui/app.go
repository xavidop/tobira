package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
	"github.com/xavidop/tobira/internal/config"
	"github.com/xavidop/tobira/internal/query"
	"github.com/xavidop/tobira/internal/secrets/keyring"
	"github.com/xavidop/tobira/internal/session"
	"github.com/xavidop/tobira/internal/tui/async"
	"github.com/xavidop/tobira/internal/tui/connform"
	"github.com/xavidop/tobira/internal/tui/detail"
	"github.com/xavidop/tobira/internal/tui/editor"
	"github.com/xavidop/tobira/internal/tui/filter"
	"github.com/xavidop/tobira/internal/tui/grid"
	"github.com/xavidop/tobira/internal/tui/help"
	"github.com/xavidop/tobira/internal/tui/keymap"
	"github.com/xavidop/tobira/internal/tui/palette"
	"github.com/xavidop/tobira/internal/tui/sidebar"
	"github.com/xavidop/tobira/internal/tui/statusbar"
	"github.com/xavidop/tobira/internal/tui/theme"
)

// connectTick is how often the screen is redrawn while a connection is
// opening, so its status dot stays current.
const connectTick = 120 * time.Millisecond

// Options configures the TUI.
type Options struct {
	Manager *session.Manager
	// Theme is auto (the default), tobira-dark or tobira-light.
	Theme string
	// Keymap is default (the default) or vim.
	Keymap string
	// Focus names a connection to expand at startup; empty for none.
	Focus  string
	Logger *slog.Logger
	// ConfigPath is the config file connections are added to, edited in and
	// removed from: the one the Manager's config was loaded from. Empty
	// disables connection management.
	ConfigPath string
	// ConfigStat identifies the config file version the Manager was loaded
	// from (config.LoadWithStat). Every write is made against it, so a file
	// changed behind the sidebar's back is a conflict, never a wrong write.
	ConfigStat config.Stat
	// Notice is a problem found at startup (for example a config file that
	// could not be read), shown as an error flash when the TUI opens. It
	// must already be redacted.
	Notice string
	// Keyring is the OS keyring behind keyring:<name> passwords. When set,
	// deleting a connection whose password ref is keyring:<name> also deletes
	// that entry. Nil leaves keyring entries alone.
	Keyring keyring.Store
	// History records the queries the editor runs. Nil records nothing.
	History HistoryStore
	// Assistant is the AI assistant; nil when AI is not configured or could
	// not be set up (then AIError says why).
	Assistant assistant.Assistant
	// AIError is why the configured assistant could not be built, already
	// redacted; nil when there is an Assistant or no ai: section.
	AIError error
	// EmbedderError is why the configured ai.embedder could not be built,
	// already redacted; nil when there is an embedder or none is set. It is
	// shown with the "text similarity needs ai.embedder" refusal.
	EmbedderError error
	// ResolveDriver, when set, makes sure the driver of a connection has
	// reported its real Info (a driver plugin is started and asked), so
	// the assistant's tools see its capabilities. Each AI turn calls it
	// first, in the turn's goroutine, never in Update; an error ends the
	// turn with that (redacted) reason.
	ResolveDriver func(ctx context.Context, conn string) error
	// Stderr receives a notice Run prints after the program has exited and
	// the screen is restored, such as quitting past a stuck apply. Nil
	// drops it.
	Stderr io.Writer
}

// tickMsg is the redraw tick that runs only while a connection is opening.
type tickMsg struct{}

// current is the collection shown in the grid.
type current struct {
	set  bool
	conn string
	ref  driver.CollectionRef
}

// Model is the root model. It lives on the Bubble Tea goroutine and is
// updated in place.
type Model struct {
	mgr *session.Manager
	log *slog.Logger

	themeName string // as chosen: auto or a theme name
	th        theme.Theme
	km        keymap.Map
	darkBG    bool

	w, h int
	lay  layout

	sb  sidebar.Model
	gr  grid.Model
	st  statusbar.Model
	fl  filter.Model
	pal palette.Model
	det *detail.Model

	focus   focusKind
	help    bool
	helpOff int

	cur        current
	src        *grid.Source
	filterText string

	ticking  bool
	wantTick bool
	focusArg string
	notice   string
	// forwarded is set when a key press reached the grid or the sidebar, which
	// track key sequences themselves; any other key clears their pending
	// first key.
	forwarded bool

	// Connection management (conn.go).
	cfgPath  string
	keyring  keyring.Store
	form     *connform.Model // the add/edit overlay
	cfgStat  config.Stat     // the config file version the Manager was last loaded from
	formLine int             // line of the entry being edited; 0 when adding
	confirm  *confirmState   // the delete confirmation overlay
	saving   bool            // a config write is in flight
	// resumeGrid is set when a grid load was cancelled for the write in
	// flight; a failed write restarts it.
	resumeGrid bool

	// stopping holds the done channels of cancelled runs that a config
	// write or reload must wait for before it may close their connection.
	stopping []<-chan struct{}

	// Query editor and result tabs (edit.go, run.go, tabs.go).
	ed      editor.Model
	edOpen  bool
	edConn  string            // the connection whose buffer the editor shows
	buffers map[string]buffer // the other connections' buffers
	tabs    []*resultTab      // run tabs; the browse grid is tab 0
	active  int               // 0 is the browse grid, i is tabs[i-1]
	limit   int               // row limit of a run (:set limit)
	runs    async.Tracker     // the live run
	running *resultTab        // the tab of the live run; nil when none
	hist    HistoryStore
	// runBuf is each connection's buffer as it was at its last run, to tell
	// an unsaved query on quit.
	runBuf map[string]string

	// Completion and history navigation (complete.go, histnav.go).
	cp     completion
	walk   histWalk
	hs     histSearch
	schema map[string]*schemaCache // fields seen per connection

	// Inline editing (inline.go, pending.go).
	pend     *pendingSet // pending edits of the open collection; nil when none
	cell     *cellEdit   // the open cell editor
	guardSt  *guardState // the y/N confirmation before pending edits are lost
	apply    *applyRun   // the :apply in flight
	applySeq uint64
	// The schema read of the open collection (inline.go describe): its
	// cancel func (nil when none runs), its id, the ids handed out, and the
	// redacted error of the last failed read.
	descCancel context.CancelFunc
	descID     uint64
	descSeq    uint64
	descErr    string
	// now is the clock (time.Now); tests set it.
	now func() time.Time
	// exitNotice is printed on stderr once the program has exited.
	exitNotice string

	// :tx, the prompts and :export (tx.go, prompt.go, export.go).
	tx        *txState // the open transaction, or its begin; nil when none
	txSeq     uint64   // ids handed to begins
	txWaitFor time.Duration
	// txWaitHook, when set, is called off the Update goroutine as a call
	// starts waiting for the calls still inside the Tx (tests only).
	txWaitHook func()
	ask        *askState  // the open y/N or transaction prompt
	export     *exportRun // the export in flight
	exportSeq  uint64

	// The AI assistant and its panel (ai.go).
	asst  assistant.Assistant
	aiErr error
	ai    aiState
	// resolveDriver is Options.ResolveDriver.
	resolveDriver func(context.Context, string) error

	// Kind views (kinds.go, chart.go, similar.go): the open collection is
	// shown as key-value rows; the chart pane; the embedder's build error.
	kv       bool
	chart    chartState
	embedErr error

	// The live tail tabs (watch.go): the ids handed to watches, and the
	// time zone event times are shown in (nil: local).
	watchSeq uint64
	loc      *time.Location
}

// New builds the root model. It does not touch the database.
func New(opts Options) (tea.Model, error) {
	if opts.Manager == nil {
		return nil, errors.New("tui: a session manager is required")
	}
	name := opts.Theme
	if name == "" {
		name = "auto"
	}
	th, err := theme.ByName(name, true)
	if err != nil {
		return nil, err
	}
	kmName := opts.Keymap
	if kmName == "" {
		kmName = "default"
	}
	km, err := keymap.ByName(kmName)
	if err != nil {
		return nil, err
	}
	if opts.Focus != "" && !hasConn(opts.Manager, opts.Focus) {
		return nil, fmt.Errorf("unknown connection %q", opts.Focus)
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Model{
		mgr: opts.Manager, log: log, resolveDriver: opts.ResolveDriver,
		themeName: name, th: th, km: km, darkBG: true,
		sb:       sidebar.New(opts.Manager, th, km),
		gr:       grid.New(th, km),
		st:       statusbar.New(th, km, statusbar.Options{}),
		fl:       filter.New(th, km),
		pal:      palette.New(th, km),
		focusArg: opts.Focus,
		notice:   opts.Notice,
		cfgPath:  opts.ConfigPath,
		cfgStat:  opts.ConfigStat,
		keyring:  opts.Keyring,
		ed:       editor.New(th, km),
		buffers:  map[string]buffer{},
		limit:    defaultLimit,
		hist:     opts.History,
		schema:   map[string]*schemaCache{},
		runBuf:   map[string]string{},
		asst:     opts.Assistant,
		aiErr:    opts.AIError,
		embedErr: opts.EmbedderError,
		chart:    newChartState(),
	}, nil
}

func hasConn(m *session.Manager, name string) bool {
	for _, n := range m.Names() {
		if n == name {
			return true
		}
	}
	return false
}

// Run runs the TUI on in and out until it quits or ctx is cancelled, then
// closes every connection. A cancelled ctx is a clean exit.
func Run(ctx context.Context, opts Options, in io.Reader, out io.Writer) error {
	model, err := New(opts)
	if err != nil {
		return err
	}
	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	final, err := p.Run()
	m, ok := final.(*Model)
	if !ok {
		m, _ = model.(*Model)
	}
	if m != nil {
		m.recordRunAtExit() // the program has stopped: no Update can race this
		m.shutdown()
		m.closeTxAtExit()
		// A cancelled export removes its temp file before it returns.
		select {
		case <-m.exportWait():
		case <-time.After(stopWait):
		}
		if ok {
			reportExit(opts.Stderr, m)
		}
	}
	if err != nil && ctx.Err() != nil && errors.Is(err, tea.ErrProgramKilled) {
		err = nil
	}
	return errors.Join(err, opts.Manager.CloseAll())
}

// reportExit prints the model's exit notice, if any, as a warning on w, the
// way the CLI reports its own warnings.
func reportExit(w io.Writer, m *Model) {
	if w == nil || m.exitNotice == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "warning: %s\n", query.EscapeControls(m.exitNotice))
}

// Init asks the terminal for its background colour when the theme is auto and
// expands the Focus connection.
func (a *Model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if a.themeName == "auto" {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	if a.focusArg != "" {
		cmds = append(cmds, a.sb.Expand(a.focusArg), a.tickCmd())
	}
	if a.notice != "" {
		cmds = append(cmds, a.flash(a.notice, true))
	}
	return tea.Batch(cmds...)
}

// Update routes msg, then refreshes the derived state: the status bar, the
// layout (and with it every component's size) and the connecting tick.
func (a *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := a.update(msg)
	if a.focus == focusAI && !a.ai.open {
		a.focus = focusGrid
	}
	if a.focus != focusEditor || !a.edOpen {
		a.leaveEditor()
	}
	a.syncAI()
	a.syncAIAsk()
	a.syncEditorConn()
	a.syncBar()
	a.relayout()
	cmd = tea.Batch(cmd, a.syncChart())
	if a.wantTick || a.anyConnecting() {
		a.wantTick = false
		cmd = tea.Batch(cmd, a.tickCmd())
	}
	return a, cmd
}

func (a *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		return nil
	case tea.BackgroundColorMsg:
		a.darkBG = msg.IsDark()
		if a.themeName == "auto" {
			a.setTheme("auto")
		}
		return nil
	case tea.ColorProfileMsg:
		plain := msg.Profile <= colorprofile.ASCII
		a.st = a.st.SetPlain(plain)
		a.sb = a.sb.SetPlain(plain)
		return nil
	case tea.KeyPressMsg:
		return a.key(msg)
	case tea.MouseClickMsg:
		return a.mouse(tea.Mouse(msg), msg)
	case tea.MouseWheelMsg:
		return a.mouse(tea.Mouse(msg), msg)
	case tea.MouseMsg:
		return nil
	case tea.PasteMsg:
		return a.paste(msg)
	case tickMsg:
		a.ticking = false
		a.sb = a.sb.Sync() // a connection may have opened, failed or closed
		return nil
	case sidebar.OpenCollectionMsg:
		return a.openCollection(msg)
	case sidebar.StatusMsg:
		return a.flash(msg.Text, msg.Err)
	case grid.PageMsg:
		return a.page(msg)
	case runDoneMsg:
		return a.runDone(msg)
	case describedMsg:
		a.described(msg)
		return nil
	case applyDoneMsg:
		return a.applyDone(msg)
	case palette.ApplyCmd:
		return a.applyPending()
	case palette.DiscardCmd:
		return a.discard()
	case guardDoneMsg:
		return a.flash(msg.text, false)
	case palette.TxCmd:
		return a.txCmd(msg)
	case txDoneMsg:
		return a.txDone(msg)
	case palette.ReadOnlyCmd:
		return a.setReadOnly(msg.On)
	case palette.ExportCmd:
		return a.startExport(msg, false)
	case exportStartMsg:
		return a.startExport(msg.cmd, msg.overwrite)
	case exportTargetMsg:
		return a.exportTarget(msg)
	case exportProgressMsg:
		return a.exportProgress(msg)
	case exportDoneMsg:
		return a.exportDone(msg)
	case proposalMsg:
		return a.loadProposal(msg.q, msg.run)
	case quitMsg:
		return a.quitWith(msg)
	case completeTickMsg:
		return a.completeTick(msg)
	case completeDoneMsg:
		return a.completeDone(msg)
	case walkDoneMsg:
		return a.walkDone(msg)
	case searchDoneMsg:
		return a.searchDone(msg)
	case editor.RefusedMsg:
		return a.flash(msg.Text, true)
	case palette.EditorCmd:
		a.openEditor()
		return nil
	case palette.ExplainCmd:
		return a.explain()
	case palette.SetLimitCmd:
		a.limit = msg.N
		return a.flash("row limit set to "+strconv.Itoa(msg.N), false)
	case grid.RowActivatedMsg:
		return a.openDetail()
	case detail.ClosedMsg:
		a.dropDetail()
		return nil
	case detail.BuiltMsg:
		if a.det == nil {
			return nil
		}
		d, cmd := a.det.Update(msg)
		a.det = &d
		return cmd
	case detail.CopiedMsg:
		return a.flash("copied "+msg.What, false)
	case filter.AppliedMsg:
		a.filterText = msg.Text
		return a.gr.SetFilter(msg.Filter)
	case filter.CancelledMsg, palette.CancelledMsg:
		return nil
	case statusbar.FlashExpiredMsg:
		var cmd tea.Cmd
		a.st, cmd = a.st.Update(msg)
		return cmd
	case palette.QuitCmd:
		return a.quit()
	case palette.ThemeCmd:
		return a.setTheme(msg.Name)
	case palette.KeymapCmd:
		return a.setKeymap(msg.Name)
	case palette.ConnectCmd:
		return a.connect(msg.Name)
	case palette.ReloadCmd:
		return a.reload()
	case palette.HelpCmd:
		a.openHelp()
		return nil
	case palette.ChartCmd:
		return a.chartCmd(msg)
	case palette.RangeCmd:
		return a.rangeCmd(msg)
	case chartDoneMsg:
		return a.chartDone(msg)
	case palette.SimilarCmd:
		return a.similarText(msg.Text)
	case palette.WatchCmd:
		return a.watchCmd(msg)
	case palette.UnwatchCmd:
		return a.unwatch()
	case watchStartedMsg:
		return a.watchStarted(msg)
	case watchEventsMsg:
		return a.watchEvents(msg)
	case palette.Unavailable:
		return a.flash(query.EscapeControls(msg.Feature+" arrives in "+msg.Milestone), false)
	case palette.AskCmd:
		return a.aiCommand(msg)
	case palette.ExplainResultsCmd:
		return a.explainResults()
	case palette.AIExecuteCmd:
		return a.aiExecute(msg.On)
	case aiEventMsg:
		return a.aiEvent(msg)
	case palette.ConnAddCmd:
		return a.openForm(connform.Add, "")
	case palette.ConnEditCmd:
		return a.openForm(connform.Edit, msg.Name)
	case palette.ConnRmCmd:
		return a.openConfirm(msg.Name)
	case connform.SavedMsg:
		return a.saveForm(msg)
	case connform.CancelledMsg:
		a.form = nil
		return nil
	case connSavedMsg:
		return a.saved(msg)
	}
	// Everything else (sidebar load results, the form's own messages, ...)
	// belongs to a component.
	var c1, c2, c3 tea.Cmd
	a.sb, c1 = a.sb.Update(msg)
	a.gr, c2 = a.gr.Update(msg)
	if a.form != nil {
		var f connform.Model
		f, c3 = a.form.Update(msg)
		a.form = &f
	}
	return tea.Batch(c1, c2, c3)
}

// ---- keys -----------------------------------------------------------------

func (a *Model) key(k tea.KeyPressMsg) tea.Cmd {
	a.forwarded = false
	defer func() {
		if !a.forwarded {
			a.gr, a.sb = a.gr.ClearSeq(), a.sb.ClearSeq()
		}
	}()
	// An error flash lasts until the next key press.
	a.st = a.st.DismissError()
	// The connection form and the delete confirmation own the keyboard: no
	// binding, not even quit, fires while one is open, and while their write
	// is in flight keys are dropped. A reload has no such pane, so the
	// keyboard stays live (quit and cancel included); only edit, delete and
	// a second reload are refused, where they would start.
	switch {
	case a.saving && (a.form != nil || a.confirm != nil):
		return nil
	case a.form != nil:
		return a.formKey(k)
	case a.confirm != nil:
		return a.confirmKey(k)
	case a.guardSt != nil:
		return a.guardKey(k)
	case a.ask != nil:
		return a.askKey(k)
	case a.cell != nil:
		return a.cellKey(k)
	}
	if a.km.Matches(k, keymap.Cancel) {
		if a.hs.active && a.focus == focusEditor && a.edOpen {
			a.endSearch(false) // like readline: abort the search, keep the session
			return nil
		}
		return a.cancelOrQuit()
	}
	// A focused text input owns every key: no bare-letter binding may fire.
	switch {
	case a.pal.Focused():
		var cmd tea.Cmd
		a.pal, cmd = a.pal.Update(k)
		return cmd
	case a.fl.Focused():
		var cmd tea.Cmd
		a.fl, cmd = a.fl.Update(k)
		return cmd
	case a.sb.FilterFocused():
		return a.updateSidebar(k)
	case a.help:
		a.helpKey(k)
		return nil
	case a.det != nil:
		if a.km.Matches(k, keymap.Back) {
			a.dropDetail()
			return nil
		}
		d, cmd := a.det.Update(k)
		a.det = &d
		return cmd
	case a.focus == focusEditor && a.edOpen:
		return a.editorKey(k)
	case a.focus == focusAI && a.ai.open:
		return a.aiKey(k)
	}

	switch {
	case a.km.Matches(k, keymap.Help):
		a.openHelp()
		return nil
	case a.km.Matches(k, keymap.Palette):
		a.pal = a.pal.Open()
		return nil
	case a.km.Matches(k, keymap.Quit):
		return a.quit()
	case a.km.Matches(k, keymap.FocusNext):
		a.cycleFocus(1)
		return nil
	case a.km.Matches(k, keymap.FocusPrev):
		a.cycleFocus(-1)
		return nil
	case a.km.Matches(k, keymap.EditorToggle):
		a.toggleEditor()
		return nil
	case a.edOpen && a.km.Matches(k, keymap.RunStatement):
		return a.runStatement()
	case a.edOpen && a.km.Matches(k, keymap.RunBuffer):
		return a.runBuffer()
	case a.km.Matches(k, keymap.Ask):
		a.toggleAI()
		return nil
	}
	if a.focus == focusSidebar {
		if cmd, ok := a.connKey(k); ok {
			return cmd
		}
		return a.updateSidebar(k)
	}
	return a.gridKey(k)
}

func (a *Model) gridKey(k tea.KeyPressMsg) tea.Cmd {
	if cmd, ok := a.tabKey(k); ok {
		return cmd
	}
	if cmd, ok := a.editKeyPress(k); ok {
		return cmd
	}
	t := a.activeTab()
	gr := a.activeGrid()
	if t != nil && t.watch != nil {
		if cmd, ok := a.watchKey(t, k); ok {
			return cmd
		}
	}
	switch {
	case a.km.Matches(k, keymap.Back):
		// esc walks back towards the sidebar: the AI panel and the editor
		// hand the focus to the grid, the grid hands it on.
		a.focus = focusSidebar
		return nil
	case a.km.Matches(k, keymap.Watch):
		return a.watchCmd(palette.WatchCmd{})
	case t != nil && t.watch == nil && t.state == tabFailed && a.km.Matches(k, keymap.FixError):
		return a.fixWithAI(t)
	case t != nil && (a.km.Matches(k, keymap.Filter) || a.km.Matches(k, keymap.Sort)):
		// A run tab holds a collected result: sorting or filtering it would
		// silently drop what the limit cut, so both stay with the browse tab.
		return a.flash("sort/filter apply to the browse tab", false)
	case t != nil && t.isText:
		a.textKey(t, k)
		return nil
	case a.km.Matches(k, keymap.FindSimilar):
		return a.findSimilar()
	case a.km.Matches(k, keymap.Filter):
		if a.src == nil || !a.src.Browser() {
			return a.flash("filtering needs a browsable collection", false)
		}
		a.fl = a.fl.Open(a.gr.Columns(), a.filterText)
		return nil
	case a.km.Matches(k, keymap.Open):
		if retry := gr.Retry(); retry != nil {
			return retry // enter on the grid's error message tries the page again
		}
		return a.openDetail()
	case a.km.Matches(k, keymap.CopyCell):
		row, ok := gr.CurrentRow()
		_, col := gr.Cursor()
		if !ok || col >= len(row) {
			return a.flash("nothing to copy", false)
		}
		return tea.Batch(tea.SetClipboard(detail.CellText(row[col])), a.flash("copied cell", false))
	case a.km.Matches(k, keymap.CopyRow):
		row, ok := gr.CurrentRow()
		if !ok {
			return a.flash("nothing to copy", false)
		}
		return tea.Batch(tea.SetClipboard(detail.RowJSON(gr.Columns(), row)), a.flash("copied row", false))
	}
	a.forwarded = true
	var cmd tea.Cmd
	*gr, cmd = gr.Update(k)
	return cmd
}

// cycleFocus moves the focus through the sidebar, the grid and, when it is
// open, the editor.
func (a *Model) cycleFocus(d int) {
	order := []focusKind{focusSidebar, focusGrid}
	if a.edOpen {
		order = append(order, focusEditor)
	}
	if a.ai.open {
		order = append(order, focusAI)
	}
	i := 0
	for j, f := range order {
		if f == a.focus {
			i = j
		}
	}
	a.focus = order[((i+d)%len(order)+len(order))%len(order)]
	if a.focus == focusAI {
		a.ai.in.Focus()
	}
}

// updateSidebar forwards msg to the sidebar. A command that may open a
// connection arms the redraw tick.
func (a *Model) updateSidebar(msg tea.Msg) tea.Cmd {
	a.forwarded = true
	var cmd tea.Cmd
	a.sb, cmd = a.sb.Update(msg)
	if cmd != nil {
		conn, _ := a.sb.Selected()
		if s := a.mgr.State(conn).Status; s != session.Ready {
			a.wantTick = true
		}
	}
	return cmd
}

func (a *Model) cancelOrQuit() tea.Cmd {
	// An apply in flight is cancelled first; its result says what was
	// written.
	if a.apply != nil {
		if a.apply.cancelled && a.stuck() {
			return a.quitStuck()
		}
		a.cancelApply()
		return a.flash("cancelling the apply…", false)
	}
	// A commit or rollback is not cancelled (its outcome would be unknown);
	// past stuckAfter ctrl+c quits anyway.
	if a.txEnding() {
		if a.txStuck() {
			return a.quitStuckTx()
		}
		return a.flash(a.waitTxText(), false)
	}
	// A run in flight is what ctrl+c is for: cancel it and nothing else.
	if record, ran := a.cancelRun(); ran {
		return tea.Batch(record, a.flash("cancelled", false))
	}
	if a.cancelTurn("cancelled") {
		return a.flash("cancelled the AI turn", false)
	}
	if a.cancelExport() {
		return a.flash("cancelling the export…", false)
	}
	if a.cancelChart() {
		return a.flash("cancelled the chart", false)
	}
	was := a.gr.CancelLoad()
	if a.sb.CancelAll() {
		was = true
	}
	if was {
		return a.flash("cancelled", false)
	}
	return a.quit()
}

// shutdown abandons every driver call in flight so nothing still uses a
// connection when the session closes it.
func (a *Model) shutdown() {
	if a.form != nil {
		a.form.CancelTest()
	}
	a.gr.CancelLoad()
	a.sb.CancelAll()
	a.runs.Cancel()
	a.running = nil
	a.cancelTurn("")
	a.cancelApply()
	a.stopDescribe()
	a.cancelExport()
	a.closeChart()
	a.stopWatchesOn("", "", "")
	if a.tx != nil && a.tx.cancel != nil {
		a.tx.cancel() // a begin in flight
	}
	a.dropDetail()
}

func (a *Model) openHelp() {
	a.help, a.helpOff = true, 0
	a.focus = focusGrid
}

func (a *Model) helpKey(k tea.KeyPressMsg) {
	page := max(a.lay.grid.h-1, 1)
	off := a.helpOff
	switch {
	case a.km.Matches(k, keymap.Back), a.km.Matches(k, keymap.Help):
		a.help = false
		return
	case a.km.Matches(k, keymap.Up):
		off--
	case a.km.Matches(k, keymap.Down):
		off++
	case a.km.Matches(k, keymap.PageUp):
		off -= page
	case a.km.Matches(k, keymap.PageDown):
		off += page
	}
	a.helpOff = min(max(off, 0), help.MaxOffset(a.km, a.lay.grid.w, a.lay.grid.h))
}

// ---- mouse ------------------------------------------------------------------

// mouse routes a click or wheel event to the pane under the pointer, with
// coordinates made relative to that pane's content (the grid's header is
// row 0).
func (a *Model) mouse(m tea.Mouse, orig tea.Msg) tea.Cmd {
	if a.pal.Focused() || a.fl.Focused() || a.sb.FilterFocused() || a.connOverlayOpen() || a.cell != nil || a.guardSt != nil || a.ask != nil {
		return nil
	}
	l := a.lay
	_, isClick := orig.(tea.MouseClickMsg)
	in := func(r rect) bool { return m.X >= r.x && m.X < r.x+r.w && m.Y >= r.y && m.Y < r.y+r.h }
	if l.ai.w > 0 && in(l.ai) {
		if w, ok := orig.(tea.MouseWheelMsg); ok {
			switch w.Button {
			case tea.MouseWheelUp:
				a.ai.scroll += 3
			case tea.MouseWheelDown:
				a.ai.scroll = max(a.ai.scroll-3, 0)
			}
		}
		if isClick {
			a.openAI()
		}
		return nil
	}
	titleOf := func(r rect) bool { return l.titles && m.Y == r.y-1 && m.X >= r.x && m.X < r.x+r.w }
	if l.showGrid && (in(l.editor) || in(l.edTitle)) {
		// The popup and the search prompt belong to the cursor's place.
		a.closePopup()
		a.endSearch(true)
		if w, ok := orig.(tea.MouseWheelMsg); ok {
			a.ed, _ = a.ed.Update(w)
			return nil
		}
		if !isClick {
			return nil
		}
		a.openEditor()
		if in(l.editor) {
			m.X -= l.editor.x
			m.Y -= l.editor.y
			a.ed, _ = a.ed.Update(tea.MouseClickMsg(m))
		}
		return nil
	}
	if l.showGrid && in(l.tabs) {
		if idx, ok := a.tabAt(m.X - l.tabs.x); ok && isClick {
			a.active = idx
			a.focus = focusGrid
			a.dropDetail()
		}
		return nil
	}

	switch {
	case l.showSidebar && (in(l.sidebar) || titleOf(l.sidebar)):
		if isClick {
			a.focus = focusSidebar
		}
		if !in(l.sidebar) {
			return nil
		}
		m.X -= l.sidebar.x
		m.Y -= l.sidebar.y
		return a.updateSidebar(rebuildMouse(orig, m))
	case l.showGrid && (in(l.grid) || titleOf(l.col)):
		if isClick {
			a.focus = focusGrid
		}
		if !in(l.grid) {
			return nil
		}
		if w, ok := orig.(tea.MouseWheelMsg); ok && (a.help || a.det != nil) {
			return a.wheelOverlay(w)
		}
		if a.help || a.det != nil {
			return nil
		}
		m.X -= l.grid.x
		m.Y -= l.grid.y
		if t := a.activeTab(); t != nil && t.isText {
			return nil
		}
		gr := a.activeGrid()
		var cmd tea.Cmd
		*gr, cmd = gr.Update(rebuildMouse(orig, m))
		return cmd
	}
	return nil
}

// wheelOverlay scrolls the help or row-detail overlay under the pointer.
func (a *Model) wheelOverlay(w tea.MouseWheelMsg) tea.Cmd {
	if a.det != nil {
		d, cmd := a.det.Update(w)
		a.det = &d
		return cmd
	}
	const step = 3
	switch w.Button {
	case tea.MouseWheelUp:
		a.helpOff -= step
	case tea.MouseWheelDown:
		a.helpOff += step
	default:
		return nil
	}
	a.helpOff = min(max(a.helpOff, 0), help.MaxOffset(a.km, a.lay.grid.w, a.lay.grid.h))
	return nil
}

func rebuildMouse(orig tea.Msg, m tea.Mouse) tea.Msg {
	if _, ok := orig.(tea.MouseWheelMsg); ok {
		return tea.MouseWheelMsg(m)
	}
	return tea.MouseClickMsg(m)
}

// ---- collections --------------------------------------------------------------

// openCollection shows a collection in the grid. The grid starts loading at
// once (cancelling the previous collection's load and dropping its late
// results), and the labels switch with it, so what is drawn always belongs to
// the collection named.
func (a *Model) openCollection(msg sidebar.OpenCollectionMsg) tea.Cmd {
	if a.pendingFor(msg.Conn, msg.Ref) {
		if cmd, held := a.guard("opening "+msg.Conn+" ▸ "+collName(msg.Ref)+" drops them", resend(msg)); held {
			return cmd
		}
	}
	if a.tx != nil && msg.Conn != a.tx.conn {
		// The editor follows the open collection to the other connection.
		if cmd, held := a.txGuard("opening "+msg.Conn+" ▸ "+collName(msg.Ref)+" moves the editor off it", resend(msg), false); held {
			return cmd
		}
	}
	conn, ok := a.mgr.Conn(msg.Conn)
	if !ok {
		return a.flashErr(msg.Conn, "connection "+strconv.Quote(msg.Conn)+" is not open")
	}
	if a.cur.conn != msg.Conn || a.cur.ref != msg.Ref {
		a.discardPending()
	}
	a.stopWatchesOn("", msg.Conn, "switched to connection "+msg.Conn) // the watches on other connections
	a.log.Debug("open collection", "conn", msg.Conn, "collection", msg.Ref.Name)
	st := a.mgr.State(msg.Conn)
	name := msg.Conn
	a.gr.CancelLoad()
	a.cur = current{set: true, conn: msg.Conn, ref: msg.Ref}
	a.closeChart()
	a.gr.SetHidden(nil)     // until Describe marks _truncated (see described)
	a.gr.SetHeaderMark(nil) // until Describe reports a wide-column key
	a.kv = isKeyValue(st.Info, msg.Type)
	a.gr.SetFormat(cellFormat(st.Info, msg.Type))
	a.focus = focusGrid
	a.dropDetail()
	a.help = false
	a.filterText = ""
	mgr := a.mgr
	a.src = grid.NewSource(conn, st.Info.Name, msg.Ref, st.ReadOnly).
		WithReadOnly(func() bool { return mgr.State(name).ReadOnly }). // :readonly applies to the next page
		WithEstimate(msg.RowEstimate).
		WithRedact(func(s string) string { return a.mgr.Redact(name, s) })
	var describe tea.Cmd
	if needsDescribe(st.Info, conn) {
		describe = a.describe(a.src)
	} else {
		a.stopDescribe()
	}
	return tea.Batch(a.gr.Open(a.src), describe)
}

func (a *Model) reload() tea.Cmd {
	if cmd, held := a.guard("reloading drops them", resend(palette.ReloadCmd{})); held {
		return cmd
	}
	if a.cfgPath != "" {
		// Only a config reload can close the transaction's connection.
		if cmd, held := a.txGuard("reloading may close its connection", resend(palette.ReloadCmd{}), false); held {
			return cmd
		}
	}
	if a.cfgPath != "" && a.saving {
		return a.flash(busyText, false)
	}
	if a.cfgPath != "" {
		// Re-read the config file: a file changed outside tobira is the
		// usual reason to reload. The reload may close any connection, so an
		// editor run is cancelled first and the reload waits for it.
		a.saving = true
		record, _ := a.cancelRun()
		a.stopLookups("")
		a.stopApplyOn("")
		a.stopExportOn("")
		a.stopAIOn("")
		a.stopChartOn("")
		a.stopWatchesOn("", "", "the config was reloaded")
		return tea.Batch(record, a.invalidateCmd(a.mgr.Names()...), a.reloadConfigCmd())
	}
	a.aiReloaded(true) // no config file to re-read, but consent never survives a :reload
	a.stopLookups("")
	inv := a.invalidateCmd(a.mgr.Names()...)
	if !a.cur.set || a.src == nil {
		return tea.Batch(inv, a.flash("nothing to reload", false))
	}
	a.filterText = ""
	a.dropDetail()
	var chartCmd tea.Cmd
	if a.chart.open {
		chartCmd = a.loadChart()
	}
	return tea.Batch(inv, a.gr.Open(a.src), chartCmd)
}

func (a *Model) page(msg grid.PageMsg) tea.Cmd {
	// Only the grid knows whether msg is the live request; its text is
	// already redacted with the issuing source's connection.
	var flash tea.Cmd
	if text, ok := a.gr.FailText(msg); ok {
		flash = a.flash(text, true)
	}
	var cmd tea.Cmd
	a.gr, cmd = a.gr.Update(msg)
	return tea.Batch(flash, cmd)
}

func (a *Model) paste(msg tea.PasteMsg) tea.Cmd {
	var cmd tea.Cmd
	switch {
	case a.pal.Focused():
		a.pal, cmd = a.pal.Update(msg)
	case a.fl.Focused():
		a.fl, cmd = a.fl.Update(msg)
	case a.sb.FilterFocused():
		a.sb, cmd = a.sb.Update(msg)
	case a.form != nil:
		var f connform.Model
		f, cmd = a.form.Update(msg)
		a.form = &f
	case a.confirm != nil:
		a.confirm.in, cmd = a.confirm.in.Update(msg)
	case a.guardSt != nil, a.ask != nil:
	case a.cell != nil:
		cmd = a.cellPaste(msg)
	case a.focus == focusAI && a.ai.open:
		a.ai.in, cmd = a.ai.in.Update(msg)
	case a.focus == focusEditor && a.edOpen && !a.help && a.det == nil:
		a.endSearch(true)
		ver, cur := a.ed.Version(), a.ed.Cursor()
		a.ed, cmd = a.ed.Update(msg)
		cmd = tea.Batch(cmd, a.afterEditorKey(false, ver, cur))
	}
	return cmd
}

func (a *Model) openDetail() tea.Cmd {
	if a.help || a.det != nil {
		return nil
	}
	gr := a.activeGrid()
	row, ok := gr.CurrentRow()
	if !ok {
		return a.flash("no row to show", false)
	}
	var d detail.Model
	if a.kv && a.activeTab() == nil {
		d = detail.NewKeyValue(gr.Columns(), row, a.th, a.km)
	} else {
		d = detail.New(gr.Columns(), row, a.th, a.km)
	}
	d = d.WithAbsent(gr.CurrentAbsent()).Resize(a.lay.grid.w, a.lay.grid.h)
	a.det = &d
	a.focus = focusGrid
	return d.Init() // builds huge values' trees in the background
}

// dropDetail closes the row detail and stops any tree it is still building.
func (a *Model) dropDetail() {
	if a.det != nil {
		a.det.Cancel()
		a.det = nil
	}
}

func (a *Model) connect(name string) tea.Cmd {
	if !hasConn(a.mgr, name) {
		return a.flash("unknown connection "+strconv.Quote(query.EscapeControls(name)), true)
	}
	if name != a.cur.conn {
		if cmd, held := a.guard("switching to connection "+name+" drops them", resend(palette.ConnectCmd{Name: name})); held {
			return cmd
		}
	}
	a.focus = focusSidebar
	a.wantTick = true
	return a.sb.Expand(name)
}

// ---- theme and keymap -----------------------------------------------------------

func (a *Model) setTheme(name string) tea.Cmd {
	th, err := theme.ByName(name, a.darkBG)
	if err != nil {
		return a.flash(err.Error(), true)
	}
	a.themeName, a.th = name, th
	a.sb = a.sb.SetTheme(th)
	a.gr = a.gr.SetTheme(th)
	a.st = a.st.SetTheme(th)
	a.pal = a.pal.SetTheme(th)
	a.fl = a.fl.SetTheme(th)
	a.ed = a.ed.SetTheme(th)
	for _, t := range a.tabs {
		t.gr = t.gr.SetTheme(th)
	}
	return nil
}

func (a *Model) setKeymap(name string) tea.Cmd {
	km, err := keymap.ByName(name)
	if err != nil {
		return a.flash(err.Error(), true)
	}
	a.km = km
	a.sb = a.sb.SetKeymap(km)
	a.gr = a.gr.SetKeymap(km)
	a.st = a.st.SetKeymap(km)
	a.pal = a.pal.SetKeymap(km)
	a.fl = a.fl.SetKeymap(km)
	a.ed = a.ed.SetKeymap(km)
	for _, t := range a.tabs {
		t.gr = t.gr.SetKeymap(km)
	}
	return nil
}

// ---- status -------------------------------------------------------------------

// flash shows text in the status bar. text must already be redacted.
func (a *Model) flash(text string, isErr bool) tea.Cmd {
	var cmd tea.Cmd
	a.st, cmd = a.st.Flash(query.EscapeControls(text), isErr)
	return cmd
}

// flashErr shows an error that may carry secrets of connection conn.
func (a *Model) flashErr(conn, text string) tea.Cmd {
	return a.flash(a.mgr.Redact(conn, text), true)
}

// syncBar copies the grid and session state into the status bar.
func (a *Model) syncBar() {
	conn := a.cur.conn
	if !a.cur.set {
		conn, _ = a.sb.Selected()
	}
	a.st = a.st.SetPane(a.barPane()).SetAI(a.asst != nil).SetPending(a.pend.count()).
		SetTx(a.tx != nil && a.tx.tx != nil).SetTxStuck(a.tx != nil && a.tx.stuck).SetActivity(a.exportStatus())
	if t := a.activeTab(); t != nil {
		s := a.mgr.State(t.conn)
		a.st = a.st.SetReadOnly(s.ReadOnly).SetStatus(s.Status).
			SetContext(t.conn, "", strconv.Itoa(a.active)+":"+t.label)
		pos := 0 // a text tab has no rows (t.rows stays 0)
		if row, _ := t.gr.Cursor(); t.rows > 0 {
			pos = row + 1
		}
		if w := t.watch; w != nil {
			pos = 0
			if w.shown() > 0 {
				pos = w.cursor + 1
			}
			a.st = a.st.SetStats(pos, w.shown(), 0, 0).SetPartial(false)
			return
		}
		a.st = a.st.SetStats(pos, t.rows, 0, t.took).SetPartial(t.truncated)
		return
	}
	s := a.mgr.State(conn)
	a.st = a.st.SetReadOnly(s.ReadOnly).SetStatus(s.Status)
	if !a.cur.set {
		a.st = a.st.SetContext(conn, "", "").SetStats(0, 0, 0, 0)
		return
	}
	gs := a.gr.Status()
	row, _ := a.gr.Cursor()
	pos := 0
	if np := a.gr.PinnedCount(); gs.Loaded > 0 && row >= np {
		pos = row - np + 1 // inserted rows are not counted
	}
	a.st = a.st.SetContext(a.cur.conn, a.cur.ref.Namespace, a.cur.ref.Name).
		SetStats(pos, gs.Loaded, int64(gs.Total), gs.Took).SetPartial(!gs.Exact)
}

// barPane is the pane whose key hints the status bar shows: the focused one,
// falling back to the grid when the focused editor or AI panel is closed.
func (a *Model) barPane() statusbar.Pane {
	switch {
	case a.focus == focusSidebar:
		return statusbar.PaneSidebar
	case a.focus == focusEditor && a.edOpen:
		return statusbar.PaneEditor
	case a.focus == focusAI && a.ai.open:
		return statusbar.PaneAI
	}
	return statusbar.PaneGrid
}

// ---- layout and drawing -----------------------------------------------------------

func (a *Model) inputView() string {
	switch {
	case a.pal.Focused():
		return a.pal.View(a.w)
	case a.fl.Focused():
		return a.fl.View(a.w)
	case a.cell != nil && !a.cell.multi():
		return a.cellView(a.w)
	}
	return ""
}

// relayout recomputes the layout and resizes every component to exactly the
// size View will draw it at.
func (a *Model) relayout() {
	inputH := 0
	if iv := a.inputView(); iv != "" {
		inputH = strings.Count(iv, "\n") + 1
	}
	overlay := a.help || a.det != nil || a.connOverlayOpen() || a.guardSt != nil || a.ask != nil || a.cell != nil && a.cell.multi()
	place := aiNone
	switch {
	case a.ai.open && a.w >= aiSideMin:
		place = aiSide
	case a.aiVisible():
		place = aiOverlay
	}
	a.lay = computeLayout(a.w, a.h, inputH, a.focus, overlay,
		colOpts{tabs: len(a.tabs) > 0 || a.edOpen, editor: a.edOpen, footer: a.edOpen && a.hs.active, ai: place, chart: a.chart.open})
	l := a.lay
	a.pal = a.pal.Resize(a.w)
	a.fl = a.fl.Resize(a.w)
	a.sb = a.sb.Resize(l.sidebar.w, l.sidebar.h)
	a.gr = a.gr.Resize(l.grid.w, l.grid.h)
	for _, t := range a.tabs {
		t.gr = t.gr.Resize(l.grid.w, l.grid.h)
	}
	if l.editor.h > 0 {
		a.ed = a.ed.Resize(l.editor.w, l.editor.h)
	}
	if a.det != nil {
		d := a.det.Resize(l.grid.w, l.grid.h)
		a.det = &d
	}
	if a.form != nil {
		f := a.form.Resize(l.grid.w, l.grid.h)
		a.form = &f
	}
	if a.cell != nil && a.cell.multi() {
		ed := a.cell.ed.Resize(l.grid.w, max(l.grid.h-1, 1))
		a.cell.ed = &ed
	}
	if l.ai.w > 0 {
		a.ai.in.SetWidth(max(l.ai.w-len(a.ai.in.Prompt)-1, 1))
		a.aiPrepare(l.ai)
	}
}

func (a *Model) tickCmd() tea.Cmd {
	if a.ticking {
		return nil
	}
	a.ticking = true
	return tea.Tick(connectTick, func(time.Time) tea.Msg { return tickMsg{} })
}

func (a *Model) anyConnecting() bool {
	for _, n := range a.mgr.Names() {
		if a.mgr.State(n).Status == session.Connecting {
			return true
		}
	}
	return false
}

// View draws the screen: exactly height lines, none wider than width. Bubble
// Tea always asks the terminal for the kitty keyboard protocol's key
// disambiguation (and xterm's modifyOtherKeys), which is what lets ctrl+enter
// reach the editor; no further enhancement is needed. f5 is the fallback for
// terminals that support neither.
func (a *Model) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (a *Model) render() string {
	l := a.lay
	if l.w < 1 || l.h < 1 {
		return ""
	}
	var out []string
	if l.bodyH > 0 {
		out = append(out, a.body(l)...)
	}
	if l.inputH > 0 {
		out = append(out, fit(a.inputView(), l.w, l.inputH)...)
	}
	out = append(out, fit(a.st.View(l.w), l.w, 1)...)
	return strings.Join(out, "\n")
}

// body draws the panes and the AI panel: bodyH lines.
func (a *Model) body(l layout) []string {
	if l.aiOverlay {
		return a.aiView(l.ai)
	}
	rows := a.panes(l)
	if l.ai.w > 0 {
		div := a.th.Styles.Dim.Render("│")
		panel := a.aiView(l.ai)
		for i := range rows {
			if i < len(panel) {
				rows[i] += div + panel[i]
			}
		}
	}
	return rows
}

// panes draws the sidebar and the main column.
func (a *Model) panes(l layout) []string {
	sbCol := func() []string {
		return a.pane(l.sidebar, "connections", a.sb.View(l.sidebar.w, l.sidebar.h, a.focus == focusSidebar), a.focus == focusSidebar, l)
	}
	grCol := func() []string { return a.column(l) }
	if !l.showSidebar || !l.showGrid {
		if l.showSidebar {
			return sbCol()
		}
		return grCol()
	}
	left, right := sbCol(), grCol()
	div := a.th.Styles.Dim.Render("│")
	rows := make([]string, len(left))
	for i := range rows {
		rows[i] = left[i] + div + right[i]
	}
	return rows
}

// pane stacks the title row (when the layout has one) over content.
func (a *Model) pane(r rect, title, content string, focused bool, l layout) []string {
	var lines []string
	if l.titles {
		lines = append(lines, a.titleLine(title, r.w, focused))
	}
	return append(lines, fit(content, r.w, r.h)...)
}

func (a *Model) titleLine(title string, w int, focused bool) string {
	st := a.th.Styles.Dim
	if focused {
		st = a.th.Styles.Title
	}
	t := query.Truncate(title, w)
	return st.Render(t) + strings.Repeat(" ", max(0, w-lipgloss.Width(t)))
}

// column draws the main column: its title row, the tab bar, the grid of the
// active tab and the editor, as the layout places them.
func (a *Model) column(l layout) []string {
	title, content := a.gridPane(l.grid)
	var lines []string
	if l.titles {
		lines = append(lines, a.titleLine(title, l.col.w, a.focus == focusGrid))
	}
	if l.tabs.h > 0 {
		lines = append(lines, fit(a.tabBar(l.tabs.w), l.tabs.w, 1)...)
	}
	lines = append(lines, fit(content, l.grid.w, l.grid.h)...)
	if l.chart.h > 0 {
		lines = append(lines, a.chartPane(l.chart)...)
	}
	if l.edTitle.h > 0 {
		lines = append(lines, a.editorTitle(l.edTitle.w))
		ed := fit(a.ed.View(l.editor.w, l.editor.h, a.focus == focusEditor), l.editor.w, l.editor.h)
		a.overlayPopup(ed, l.editor.w, l.editor.h)
		lines = append(lines, ed...)
	}
	if l.edFooter.h > 0 {
		lines = append(lines, a.searchPrompt(l.edFooter.w))
	}
	return lines
}

// gridPane returns the grid area's title and content: an overlay when one is
// open, otherwise the grid or a hint.
func (a *Model) gridPane(r rect) (title, content string) {
	crumb := a.breadcrumb()
	switch {
	case a.form != nil:
		return a.form.Title(), a.form.View()
	case a.confirm != nil:
		return "delete connection", a.confirmView(r.w, r.h)
	case a.guardSt != nil:
		return "unsaved changes", a.guardView(r.w, r.h)
	case a.ask != nil:
		return a.askPaneTitle(), a.askView(r.w, r.h)
	case a.cell != nil && a.cell.multi():
		return a.cellPane(r.w, r.h)
	case a.help:
		return "help", help.ViewAt(a.km, a.th, r.w, r.h, a.helpOff)
	case a.det != nil:
		if t := a.activeTab(); t != nil {
			crumb = query.EscapeControls(t.conn) + " ▸ " + t.label
		}
		return crumb + " · row detail", a.det.View(r.w, r.h)
	}
	if conn, ok := a.failedConn(); ok {
		return "connection " + query.EscapeControls(conn), a.failedView(conn, r.w)
	}
	if t := a.activeTab(); t != nil {
		return a.tabTitle(t), a.tabView(t, r.w, r.h)
	}
	if !a.cur.set {
		return "no collection open", a.welcome()
	}
	title = crumb
	if label := a.rowsLabel(); label != "" {
		title += " · " + label
	}
	return title, a.gr.View(r.w, r.h, a.focus == focusGrid)
}

// failedConn reports the selected connection when it has failed and the grid
// is empty or shows one of its collections.
func (a *Model) failedConn() (string, bool) {
	conn, ok := a.sb.SelectedConn()
	if !ok || a.mgr.State(conn).Status != session.Failed {
		return "", false
	}
	if a.cur.set && a.cur.conn != conn {
		return "", false
	}
	return conn, true
}

// failedView explains why conn failed, with the full redacted error wrapped
// to w columns.
func (a *Model) failedView(conn string, w int) string {
	st := a.th.Styles
	state := a.mgr.State(conn)
	err := query.EscapeControls(state.Err)
	if state.Broken {
		edit := a.km.Binding(keymap.ConnEdit).Help().Key
		del := a.km.Binding(keymap.ConnRm).Help().Key
		return st.Error.Render(ansi.Wrap("connection "+query.EscapeControls(conn)+" is misconfigured:", w, "")) +
			"\n" + ansi.Wrap(err, w, "") + "\n\n" +
			st.Dim.Render(ansi.Wrap("press "+edit+" to fix it or "+del+" to delete it", w, ""))
	}
	head := st.Error.Render(ansi.Wrap("connection "+query.EscapeControls(conn)+" failed:", w, ""))
	return head + "\n" + ansi.Wrap(err, w, "") + "\n\n" +
		st.Dim.Render(ansi.Wrap("press enter to retry", w, ""))
}

func (a *Model) breadcrumb() string {
	var parts []string
	for _, p := range []string{a.cur.conn, a.cur.ref.Namespace, a.cur.ref.Name} {
		if p != "" {
			parts = append(parts, query.EscapeControls(p))
		}
	}
	return strings.Join(parts, " ▸ ")
}

func (a *Model) rowsLabel() string {
	s := a.gr.Status()
	switch {
	case s.Exact:
		return commas(s.Total) + " rows"
	case s.Total > 0:
		return "~" + commas(s.Total) + " rows"
	case s.Loaded > 0:
		return commas(s.Loaded) + "+ rows"
	case s.Loading:
		return "loading…"
	}
	return ""
}

func commas(n int) string {
	s := strconv.Itoa(n)
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			sb.WriteByte(',')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}
