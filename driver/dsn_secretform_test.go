package driver

import (
	"strings"
	"testing"
)

// The non-password secret keys (token, secret, sig, sas, credentials,
// api_key, key …) are masked where a DSN puts them, as key=value, but not
// as a JSON field or a YAML "name: value": a watched change event or a
// stored JSON query keeps its data. Password keys are masked everywhere.
func TestRedactMessageSecretKeysOnlyInKeyValueForm(t *testing.T) {
	keep := []string{
		// A watch line (a change event).
		`{"op":"update","token":"t-123","sig":"abc","secret":"s1","sas":"x","credentials":"c1","api_key":"a1","apikey":"a2","access_token":"at","private_key":"pk"}`,
		`{"payload": {"Token": "t-123", "SIG" : "abc"}}`,
		// A stored JSON query (MongoDB, Firestore, Qdrant).
		`{"collection":"sessions","filter":{"token":"t-123"}}`,
		// YAML-ish text.
		"token: t-123\nsecret: s1",
	}
	for _, s := range keep {
		if got := RedactMessage(s, ConnConfig{}); got != s {
			t.Errorf("RedactMessage(%q) = %q, want it unchanged", s, got)
		}
	}
	mask := []struct{ text, secret string }{
		// DSN contexts: URL query, key/value and ADO DSNs quoted in a message.
		{"open bigquery://p?credentials=Cr3dZq9x&x=1: refused", "Cr3dZq9x"},
		{"dial https://h/x?a=1&sig=S1gZq9xx failed", "S1gZq9xx"},
		{"bad conn string Server=h;AccountKey=AcKZq9x;Database=d", "AcKZq9x"},
		{"bad conn string host=h token=T0kZq9x dbname=d", "T0kZq9x"},
		{"history: select 1 -- api_key=ApKZq9x", "ApKZq9x"},
		// Password keys, in every form.
		{`{"op":"insert","password":"Pw9Zq9x"}`, "Pw9Zq9x"},
		{`{"pwd": "Pw8Zq9x"}`, "Pw8Zq9x"},
		{"password: Pw7Zq9x", "Pw7Zq9x"},
		{"update users set password='Pw6Zq9x'", "Pw6Zq9x"},
	}
	for _, m := range mask {
		if got := RedactMessage(m.text, ConnConfig{}); strings.Contains(got, m.secret) {
			t.Errorf("RedactMessage(%q) = %q leaks %q", m.text, got, m.secret)
		}
	}
	// RedactDSN masks the secret keys in every DSN form.
	for _, dsn := range []string{"scheme://h/db?token=T0kZq9x", "host=h secret=T0kZq9x", "Server=h;sig=T0kZq9x"} {
		if got := RedactDSN(dsn); strings.Contains(got, "T0kZq9x") {
			t.Errorf("RedactDSN(%q) = %q leaks the secret", dsn, got)
		}
	}
}
