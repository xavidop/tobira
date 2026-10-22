package cassandra

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A password over plaintext to a host that is not loopback needs
// allow_plaintext=1; Amazon Keyspaces is TLS only.
func TestPasswordPlaintextRule(t *testing.T) {
	pw := driver.NewSecret("s3cret-pw")
	for _, dsn := range []string{
		"cassandra://app@db.example.com/ks",
		"cassandra://app@db.example.com/ks?allow_plaintext=0",
		"cassandra://app@127.0.0.1,db.example.com/ks",
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: dsn, Password: pw})
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "allow_plaintext=1") {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax naming allow_plaintext=1", dsn, err)
		}
	}
	_, err := parseDSN(driver.ConnConfig{DSN: "cassandra://app@cassandra.us-east-1.amazonaws.com:9142/ks?allow_plaintext=1", Password: pw})
	if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("Keyspaces in clear = %v, want a refusal", err)
	}
	for _, cfg := range []driver.ConnConfig{
		{DSN: "cassandra://app@db.example.com/ks?allow_plaintext=1", Password: pw},
		{DSN: "cassandra://app@db.example.com/ks?tls=true", Password: pw},
		{DSN: "cassandra://app@localhost,127.0.0.2/ks", Password: pw},
		{DSN: "cassandra://db.example.com/ks"}, // no password: nothing to protect
	} {
		if _, err := parseDSN(cfg); err != nil {
			t.Errorf("parseDSN(%q): %v", cfg.DSN, err)
		}
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "cassandra://h/ks?allow_plaintext=maybe"}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("allow_plaintext=maybe = %v, want ErrSyntax", err)
	}
}
