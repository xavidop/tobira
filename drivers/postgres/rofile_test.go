package postgres

import "testing"

func TestServerFileFunction(t *testing.T) {
	for text, want := range map[string]string{
		"select lo_export(16384, '/tmp/x')":                         "lo_export",
		"SELECT LO_IMPORT('/etc/passwd')":                           "lo_import",
		"select pg_catalog.lo_unlink(1)":                            "lo_unlink",
		"select pg_file_write('x', 'y', false)":                     "pg_file_write",
		"select pg_file_rename('a', 'b')":                           "pg_file_rename",
		"select pg_file_unlink('a')":                                "pg_file_unlink",
		"select * from pg_logdir_ls()":                              "pg_logdir_ls",
		"select lo_export (1, '/tmp/x')":                            "lo_export",
		"select/**/lo_export(1,'/tmp/x')":                           "lo_export",
		`select "lo_export"(1, '/tmp/x')`:                           "lo_export",
		`select "pg_catalog"."lo_export"(1, '/tmp/x')`:              "lo_export",
		`select U&"lo\005fexport"(1, '/tmp/x')`:                     "lo_export",
		`select u&"lo!005fexport" uescape '!' (1, '/tmp/x')`:        "lo_export",
		`select U&"\+00006Co_export"(1, '/tmp/x')`:                  "lo_export",
		"explain analyze select lo_export(1, '/tmp/x')":             "lo_export",
		"with x as (select lo_export(1, '/tmp/x')) select * from x": "lo_export",
		"select 1; select lo_export(1, '/tmp/x')":                   "lo_export",
		"select 'lo_export(1)'":                                     "",
		"select $$lo_export$$":                                      "",
		"select $t$ lo_export(1) $t$":                               "",
		"select 1 -- lo_export(1)\n":                                "",
		"select 1 /* lo_export(1) */":                               "",
		"select E'\\' lo_export(1)'":                                "",
		"select lo_exporter(1)":                                     "",
		"select my_lo_export(1)":                                    "",
		"select 1":                                                  "",
	} {
		if got, _ := serverFileFunction(text); got != want {
			t.Errorf("serverFileFunction(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestIsCopy(t *testing.T) {
	for text, want := range map[string]bool{
		"copy t to '/tmp/x'": true, "COPY (select 1) TO PROGRAM 'touch /tmp/x'": true,
		"/* c */ copy t to stdout": true, ";\ncopy t from stdin": true,
		"select 'copy'": false, "copyx": false, "select 1": false, "-- copy\nselect 1": false,
	} {
		if got := isCopy(text); got != want {
			t.Errorf("isCopy(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestIsDO(t *testing.T) {
	for text, want := range map[string]bool{
		"DO $$ begin perform 1; end $$": true, "/* c */ do $$ $$": true, ";\ndo language plpgsql $$ $$": true,
		"select 'do'": false, "done": false, "-- do\nselect 1": false,
	} {
		if got := isDO(text); got != want {
			t.Errorf("isDO(%q) = %v, want %v", text, got, want)
		}
	}
}

// With standard_conforming_strings off, a backslash escapes the quote in
// '…'. The guards lex text both ways and refuse when either lexing does.
func TestGuardsScanBothStringLexings(t *testing.T) {
	// Standard lexing hides lo_export inside '\'' … ' ; backslash lexing sees it.
	if got, _ := serverFileFunction(`select '\'', lo_export(16384,'/tmp/x')`); got != "lo_export" {
		t.Errorf("serverFileFunction: hidden from standard lexing: got %q", got)
	}
	// Backslash lexing hides it; standard lexing sees it.
	if got, _ := serverFileFunction(`select 'a\', lo_export(1,'/x') --'`); got != "lo_export" {
		t.Errorf("serverFileFunction: hidden from backslash lexing: got %q", got)
	}
	if !hasTopLevelInto(`select '\'' into t`) {
		t.Error("hasTopLevelInto: INTO hidden from standard lexing")
	}
	if !hasTopLevelInto(`select 'a\' into t --'`) {
		t.Error("hasTopLevelInto: INTO hidden from backslash lexing")
	}
	if !endsWithChain(`rollback '\'' and chain`) {
		t.Error("endsWithChain: AND CHAIN hidden from standard lexing")
	}
	if !endsWithChain(`rollback 'a\' and chain --'`) {
		t.Error("endsWithChain: AND CHAIN hidden from backslash lexing")
	}
	// ROLLBACK TO stays inside the Tx only when both lexings agree.
	for text, want := range map[string]bool{
		"rollback to x":            true,
		`rollback '\'' to x`:       false, // standard lexing sees no TO
		`rollback 'a\' to x '`:     false, // backslash lexing sees no TO
		`rollback to x -- it's \'`: true,
	} {
		if got := rollsBackToSavepoint(text); got != want {
			t.Errorf("rollsBackToSavepoint(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestChangesStringGUC(t *testing.T) {
	for text, want := range map[string]string{
		"SET standard_conforming_strings = off":                             "standard_conforming_strings",
		"set local standard_conforming_strings to off":                      "standard_conforming_strings",
		"SET SESSION backslash_quote = on":                                  "backslash_quote",
		`set "escape_string_warning" = off`:                                 "escape_string_warning",
		"reset standard_conforming_strings":                                 "standard_conforming_strings",
		"select set_config('standard_conforming_strings', 'off', false)":    "standard_conforming_strings",
		"select pg_catalog.SET_CONFIG('Backslash_Quote', 'on', true)":       "backslash_quote",
		`select '\'', set_config('standard_conforming_strings','off',true)`: "standard_conforming_strings",
		"show standard_conforming_strings":                                  "",
		"select current_setting('standard_conforming_strings')":             "",
		"set search_path = public":                                          "",
		"select set_config('search_path', 'x', true)":                       "",
		"select 'set standard_conforming_strings = off'":                    "",
	} {
		if got, _ := changesStringGUC(text); got != want {
			t.Errorf("changesStringGUC(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestServerFileFunctionNonASCIIDollarTag(t *testing.T) {
	// PostgreSQL allows bytes >= 0x80 in a dollar-quote tag, so $é$'$é$ is
	// one string and the lo_export call after it runs. An ASCII-only reading
	// sees a '…' string hiding the call instead.
	for _, text := range []string{
		`select $é$'$é$, lo_export(1, '/tmp/x') --'`,
		`select $é1$'$é1$, pg_catalog.lo_import('/etc/passwd') --'`,
		`select $a$'$a$, lo_export(1, '/tmp/x') --'`,
	} {
		if _, ok := serverFileFunction(text); !ok {
			t.Errorf("serverFileFunction(%q) found nothing", text)
		}
	}
	// The ASCII-only reading still counts, so a call it sees stays refused.
	if _, ok := serverFileFunction(`select $é$ lo_export(1) $é$`); !ok {
		t.Error("serverFileFunction: a refusal of the ASCII-tag reading was lost")
	}
}
