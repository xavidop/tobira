package mongodb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestInfo(t *testing.T) {
	info := New().Info()
	if info.Name != "mongodb" || info.Kind != driver.KindDocument || info.Dialect == "" {
		t.Errorf("info = %+v", info)
	}
	for _, s := range []string{"mongodb", "mongodb+srv"} {
		found := false
		for _, x := range info.Schemes {
			found = found || x == s
		}
		if !found {
			t.Errorf("scheme %q missing from %v", s, info.Schemes)
		}
	}
	if !driver.HasCapability(info, driver.CapReadOnly) {
		t.Error("CapReadOnly not declared")
	}
}

func TestBuildOptions(t *testing.T) {
	t.Run("default database from the path", func(t *testing.T) {
		_, db, err := buildOptions(driver.ConnConfig{DSN: "mongodb://localhost:27017/shop?directConnection=true"})
		if err != nil || db != "shop" {
			t.Fatalf("db %q err %v", db, err)
		}
		_, db, err = buildOptions(driver.ConnConfig{DSN: "mongodb://localhost"})
		if err != nil || db != "test" {
			t.Fatalf("no path: db %q err %v", db, err)
		}
	})
	t.Run("password override", func(t *testing.T) {
		opts, _, err := buildOptions(driver.ConnConfig{DSN: "mongodb://ada:old@localhost/x", Password: driver.NewSecret("new")})
		if err != nil {
			t.Fatal(err)
		}
		if opts.Auth == nil || opts.Auth.Username != "ada" || opts.Auth.Password != "new" {
			t.Errorf("auth = %+v", opts.Auth)
		}
	})
	t.Run("password without a user", func(t *testing.T) {
		_, _, err := buildOptions(driver.ConnConfig{DSN: "mongodb://localhost/x", Password: driver.NewSecret("pw")})
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("want ErrSyntax, got %v", err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for _, cfg := range []driver.ConnConfig{
			{DSN: "postgres://localhost/x"},
			{DSN: "not a dsn"},
			{DSN: "mongodb://localhost/x", Options: map[string]string{"k": "v"}},
		} {
			if _, _, err := buildOptions(cfg); err == nil {
				t.Errorf("buildOptions(%+v): want an error", cfg)
			}
		}
	})
}

func TestOpenErrorsNeverShowThePassword(t *testing.T) {
	const secret = "s3cr3t-Pa55"
	for _, cfg := range []driver.ConnConfig{
		{DSN: "mongodb://ada:" + secret + "@127.0.0.1:1/x?serverSelectionTimeoutMS=200&connectTimeoutMS=200"},
		{DSN: "mongodb://ada@127.0.0.1:1/x?serverSelectionTimeoutMS=200&connectTimeoutMS=200", Password: driver.NewSecret(secret)},
		{DSN: "mongodb://ada:" + secret + "@127.0.0.1:1/x?bogusOption=%zz"},
		{DSN: "mongodb://ada:" + secret + "@127.0.0.1:1/x", Options: map[string]string{"k": "v"}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := New().Open(ctx, cfg)
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%s) succeeded", driver.RedactDSN(cfg.DSN))
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("Open error shows the password: %v", err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name || de.Op != "open" {
			t.Errorf("want a *driver.Error for open, got %#v", err)
		}
	}
}

// Secrets carried in URI options other than the password are masked too,
// in the DSN's spelling and decoded.
func TestScrubErrorMasksURIOptionSecrets(t *testing.T) {
	const token, keyPw, pw = "AwsTok3n-xyz", "KeyFileP4ss", "pa55-word"
	for _, tokInDSN := range []string{token, strings.ReplaceAll(token, "-", "%2D")} {
		dsn := "mongodb://ada:" + pw + "@h/x?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:" + tokInDSN +
			",SERVICE_NAME:svc&tlsCertificateKeyFilePassword=" + keyPw + "&sslClientCertificateKeyPassword=" + keyPw
		cfg := driver.ConnConfig{DSN: dsn}
		for _, msg := range []string{
			"error parsing uri " + dsn,
			"bad property AWS_SESSION_TOKEN:" + token,
			"token " + tokInDSN + " and key password " + keyPw,
		} {
			err := scrubError(driver.Wrap(driver.ErrSyntax, Name, "open", errors.New(msg)), cfg)
			for _, secret := range []string{token, tokInDSN, keyPw, pw} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%q leaks %q: %v", msg, secret, err)
				}
			}
			if !strings.Contains(err.Error(), "SERVICE_NAME:svc") && strings.Contains(msg, "SERVICE_NAME") {
				t.Errorf("a non-secret property was masked: %v", err)
			}
		}
	}
}
