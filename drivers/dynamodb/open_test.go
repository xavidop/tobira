package dynamodb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	type want struct{ region, endpoint, profile, accessKeyID, secret string }
	for _, tc := range []struct {
		dsn      string
		password string
		want     want
	}{
		{"dynamodb://us-east-1", "", want{region: "us-east-1"}},
		{"dynamodb://", "", want{}},
		{"dynamodb:///", "", want{}},
		{"DynamoDB://eu-west-3?profile=prod", "", want{region: "eu-west-3", profile: "prod"}},
		{"dynamodb://us-east-1?endpoint=http://localhost:8000", "", want{region: "us-east-1", endpoint: "http://localhost:8000"}},
		{"dynamodb://us-east-1?endpoint=http%3A%2F%2F127.0.0.1%3A8000", "", want{region: "us-east-1", endpoint: "http://127.0.0.1:8000"}},
		{"dynamodb://AKIAEXAMPLE@us-east-1", "s3cr3t-key", want{region: "us-east-1", accessKeyID: "AKIAEXAMPLE", secret: "s3cr3t-key"}},
		{"dynamodb://AKIAEXAMPLE:dsn-secret@us-east-1", "", want{region: "us-east-1", accessKeyID: "AKIAEXAMPLE", secret: "dsn-secret"}},
		{"dynamodb://AKIAEXAMPLE:dsn-secret@us-east-1", "ref-secret", want{region: "us-east-1", accessKeyID: "AKIAEXAMPLE", secret: "ref-secret"}},
	} {
		got, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Password: driver.NewSecret(tc.password)})
		if err != nil {
			t.Errorf("%s: %v", tc.dsn, err)
			continue
		}
		if got.region != tc.want.region || got.endpoint != tc.want.endpoint || got.profile != tc.want.profile ||
			got.accessKeyID != tc.want.accessKeyID || got.secret.Reveal() != tc.want.secret {
			t.Errorf("%s: got %+v (secret %v), want %+v", tc.dsn, got, got.secret.Reveal() == tc.want.secret, tc.want)
		}
	}
}

func TestParseDSNRefuses(t *testing.T) {
	const secret = "VERY-SECRET-KEY-123"
	for _, tc := range []struct {
		dsn, password string
		kind          error
	}{
		{"postgres://x", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?bogus=1", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?profile=a&profile=b", "", driver.ErrSyntax},
		{"dynamodb://us_east_1!", "", driver.ErrSyntax},
		{"dynamodb://us-east-1:8000", "", driver.ErrSyntax},
		{"dynamodb://us-east-1/path", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?endpoint=ftp://x", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?endpoint=http://", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?endpoint=http://u:" + secret + "@host", "", driver.ErrSyntax},
		{"dynamodb://us-east-1?endpoint=http://host/?q=1", "", driver.ErrSyntax},
		{"dynamodb://AKIAEXAMPLE@us-east-1", "", driver.ErrSyntax},
		{"dynamodb://us-east-1", secret, driver.ErrSyntax},
		{"dynamodb://AKIAEXAMPLE@us-east-1?profile=p", secret, driver.ErrSyntax},
		{"dynamodb://AKIA:" + secret + "%zz@us-east-1", "", driver.ErrSyntax},
		{"dynamodb://AKIA:" + secret + "@us-east-1?bogus=1", "", driver.ErrSyntax},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Password: driver.NewSecret(tc.password)})
		if !errors.Is(err, tc.kind) {
			t.Errorf("%s: want %v, got %v", tc.dsn, tc.kind, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error shows the secret: %v", tc.dsn, err)
		}
	}
}

func TestOpenRefusesOptions(t *testing.T) {
	_, err := New().Open(context.Background(), driver.ConnConfig{DSN: "dynamodb://us-east-1", Options: map[string]string{"x": "y"}})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
}

func TestSecretsAreMasked(t *testing.T) {
	c := &conn{secrets: secretList("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "FwoGZXIvYXdzEJr//////////session", "ab")}
	err := c.fail(context.Background(), "query", errors.New("boom wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY and FwoGZXIvYXdzEJr//////////session ab"))
	msg := err.Error()
	if strings.Contains(msg, "wJalrXUtnFEMI") || strings.Contains(msg, "FwoGZXIvYXdz") {
		t.Errorf("secret in error: %s", msg)
	}
	if !strings.Contains(msg, "ab") {
		t.Errorf("a 2-character value was masked, mangling the message: %s", msg)
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Driver != Name {
		t.Errorf("want a *driver.Error, got %T", err)
	}
}
