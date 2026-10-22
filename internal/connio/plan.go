package connio

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/config"
)

// Candidate is one entry an import source holds.
type Candidate struct {
	// Label says where the entry is in the source: its name there, or
	// "line 3". It is shown in the plan (escaped and redacted).
	Label string
	// Conn is the connection to add. Conn.Name is the name the source
	// gives, not yet sanitised. Conn.Password is a secret reference or "".
	Conn config.Connection
	// Password is a plaintext password the source holds. It is offered for
	// the keyring and never printed or written to the config; Secret keeps
	// it out of any formatting.
	Password driver.Secret
	// Skip says why the entry cannot be imported; "" when it can.
	Skip string
	// Notes are remarks for the plan, such as dropped parameters.
	Notes []string
}

// Action is what an import does with one entry.
type Action string

// The actions of a plan entry.
const (
	ActionAdd    Action = "add"
	ActionRename Action = "rename"
	ActionSkip   Action = "skip"
)

// Entry is one line of an import plan.
type Entry struct {
	Action Action
	// Label is the candidate's label, redacted.
	Label string
	// Original is the name the source gives, redacted; From differs from
	// it when sanitising changed it.
	Original string
	// From is the sanitised name the source asked for; Conn.Name differs
	// from it on a rename.
	From string
	// Conn is the connection to add (zero for a skip).
	Conn config.Connection
	// Password is the plaintext password found, for the keyring only.
	Password driver.Secret
	// Reason says why the entry is skipped.
	Reason string
	Notes  []string
}

var invalidNameRun = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Sanitize makes name fit the config's name rule (letters, digits, '.',
// '_' and '-'): each run of other characters becomes '-', and leading and
// trailing '-' go. An empty result becomes fallback, or "imported".
func Sanitize(name, fallback string) string {
	s := strings.Trim(invalidNameRun.ReplaceAllString(name, "-"), "-")
	if s == "" {
		s = strings.Trim(invalidNameRun.ReplaceAllString(fallback, "-"), "-")
	}
	if s == "" {
		s = "imported"
	}
	return s
}

// ReasonUnavailable is the skip reason of a connection whose driver this
// build does not have.
const ReasonUnavailable = "driver not available in this build"

// Plan decides, in order, what happens to each candidate. Names are
// sanitised; a name in taken, or one an earlier entry of the batch got, is
// renamed to name+suffix, then name+suffix-2, -3 and so on. A candidate
// whose driver available rejects (nil accepts all) is skipped, or, with
// keepUnavailable (a tobira export, whose driver may be a plugin not
// installed here), added with a note. One that would not pass the writer's
// checks is skipped with the reason. Labels and names are redacted against
// the candidate's own password.
func Plan(cands []Candidate, taken []string, suffix string, available func(config.Connection) bool, keepUnavailable bool) []Entry {
	used := map[string]bool{}
	for _, n := range taken {
		used[n] = true
	}
	var plan []Entry
	for _, c := range cands {
		redact := func(s string) string {
			return driver.RedactMessage(s, driver.ConnConfig{Password: c.Password})
		}
		e := Entry{Label: redact(c.Label), Password: c.Password, Notes: c.Notes}
		if c.Skip != "" {
			e.Action, e.Reason = ActionSkip, redact(c.Skip)
			plan = append(plan, e)
			continue
		}
		conn := c.Conn
		if available != nil && !available(conn) {
			if !keepUnavailable {
				e.Action, e.Reason = ActionSkip, ReasonUnavailable
				plan = append(plan, e)
				continue
			}
			name := conn.Driver
			if name == "" {
				name = driver.Scheme(conn.DSN)
			}
			e.Notes = append(slices.Clip(e.Notes), "driver "+name+" not available in this build; install the plugin")
		}
		orig := redact(conn.Name)
		want := Sanitize(orig, conn.Driver)
		conn.Name = want
		if err := config.ValidateForImport(conn); err != nil {
			e.Action = ActionSkip
			e.Reason = driver.RedactMessage(err.Error(), driver.ConnConfig{DSN: conn.DSN, Password: c.Password})
			plan = append(plan, e)
			continue
		}
		name := want
		if used[name] {
			name = want + suffix
			for n := 2; used[name]; n++ {
				name = fmt.Sprintf("%s%s-%d", want, suffix, n)
			}
		}
		used[name] = true
		conn.Name = name
		e.Action, e.Original, e.From, e.Conn = ActionAdd, orig, want, conn
		if name != want {
			e.Action = ActionRename
		}
		plan = append(plan, e)
	}
	return plan
}
