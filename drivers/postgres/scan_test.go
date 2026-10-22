package postgres

import "testing"

func TestHasTopLevelInto(t *testing.T) {
	for text, want := range map[string]bool{
		"select 1 as a into t":                               true,
		"SELECT 1 /* into */ FROM t":                         false,
		"select 'into' as x":                                 false,
		"select \"into\" from t":                             false,
		"select $$ into $$":                                  false,
		"select E'it\\'s into' ":                             false,
		"with x as (select 1) insert into t select * from x": true,
		"with x as (insert into t values (1) returning *) select * from x": false,
		"select 1 -- into\n": false,
	} {
		if got := hasTopLevelInto(text); got != want {
			t.Errorf("hasTopLevelInto(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestEndsWithChain(t *testing.T) {
	for text, want := range map[string]bool{
		"commit and chain": true, "ROLLBACK/**/AND CHAIN": true, ";end and chain": true,
		"commit": false, "commit and no chain": false, "rollback chain": false, "commit and chain_x": false, "select 'and chain'": false, "commit -- and chain": false,
	} {
		if got := endsWithChain(text); got != want {
			t.Errorf("endsWithChain(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestRollsBackToSavepoint(t *testing.T) {
	for text, want := range map[string]bool{
		"rollback to x": true, "ROLLBACK TO SAVEPOINT x": true, "rollback work to x": true, ";rollback transaction to x": true,
		"rollback": false, "rollback; to x": false, "rollback and chain": false, "rollback work and chain": false,
	} {
		if got := rollsBackToSavepoint(text); got != want {
			t.Errorf("rollsBackToSavepoint(%q) = %v, want %v", text, got, want)
		}
		if got, want := isTxControl(text), !want; got != want {
			t.Errorf("isTxControl(%q) = %v, want %v", text, got, want)
		}
	}
}
