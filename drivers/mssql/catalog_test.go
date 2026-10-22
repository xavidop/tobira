package mssql

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestResolveNamespace(t *testing.T) {
	c := &conn{loginDB: "tobira", defaultSchema: "dbo"}
	for _, tc := range []struct {
		ns, db, schema string
	}{
		{"", "tobira", "dbo"},
		{"dbo", "tobira", "dbo"},
		{"sales", "tobira", "sales"},
		{"other.dbo", "other", "dbo"},
		{"[my.db].dbo", "my.db", "dbo"},
		{"[a]]b].[c.d]", "a]b", "c.d"},
		{"[x]", "tobira", "x"},
	} {
		db, schema, err := c.resolveNS(tc.ns)
		if err != nil || db != tc.db || schema != tc.schema {
			t.Errorf("resolveNS(%q) = %q, %q, %v; want %q, %q", tc.ns, db, schema, err, tc.db, tc.schema)
		}
	}
	for _, ns := range []string{"a.b.c", "a.", ".b", "[a", "a[b", "[a]b", "a..b"} {
		if _, _, err := c.resolveNS(ns); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("resolveNS(%q) = %v, want ErrSyntax", ns, err)
		}
	}
}

func TestCanonicalNamespaceRoundTrips(t *testing.T) {
	c := &conn{loginDB: "tobira", defaultSchema: "dbo"}
	for _, tc := range []struct {
		db, schema, want string
	}{
		{"tobira", "dbo", "dbo"},
		{"TOBIRA", "sales", "sales"},
		{"other", "dbo", "other.dbo"},
		{"my.db", "dbo", "[my.db].dbo"},
		{"a]b", "c.d", "[a]]b].[c.d]"},
		{"tobira", "we[ird", "[we[ird]"},
	} {
		got := c.canonicalNS(tc.db, tc.schema)
		if got != tc.want {
			t.Errorf("canonicalNS(%q, %q) = %q, want %q", tc.db, tc.schema, got, tc.want)
		}
		db, schema, err := c.resolveNS(got)
		wantDB := tc.db
		if strings.EqualFold(tc.db, c.loginDB) {
			wantDB = c.loginDB
		}
		if err != nil || db != wantDB || schema != tc.schema {
			t.Errorf("resolveNS(%q) = %q, %q, %v; want %q, %q back", got, db, schema, err, tc.db, tc.schema)
		}
	}
}

func TestUseSQL(t *testing.T) {
	c := &conn{loginDB: "tobira", defaultSchema: "dbo"}
	for ns, want := range map[string]string{"": "", "dbo": "", "Tobira.dbo": "", "other.dbo": "USE [other]", "[we]]ird].s": "USE [we]]ird]"} {
		got, err := c.useSQL(ns)
		if err != nil || got != want {
			t.Errorf("useSQL(%q) = %q, %v; want %q", ns, got, err, want)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("a]b"); got != "[a]]b]" {
		t.Errorf("quoteIdent = %q", got)
	}
	if lr := lex("SELECT * FROM " + quoteIdent("x]; DELETE FROM t --")); lr.bad != "" || slices.ContainsFunc(lr.toks, func(t token) bool { return t.text == "DELETE" }) {
		t.Errorf("a quoted name leaked into the token stream: %+v", lr)
	}
}
