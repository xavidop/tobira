package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/internal/config"
)

func TestSSHResolvesItsRefs(t *testing.T) {
	r := Resolver{Getenv: func(string) string { return "" }, Resolve: func(_ context.Context, ref string) ([]byte, error) {
		switch ref {
		case "env:KEY_PASS":
			return []byte("pass\n"), nil
		case "env:SSH_PW":
			return []byte("pw"), nil
		}
		return nil, errors.New("unexpected ref " + ref)
	}}
	c := config.Connection{Name: "prod", DSN: "postgres://app@db:5432/app",
		SSH: &config.SSH{Host: "bastion:2222", Port: 22, User: "deploy", Key: "/k/id", Passphrase: "env:KEY_PASS", Password: "env:SSH_PW", KnownHosts: "/k/kh"}}
	got, err := r.SSH(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "bastion:2222" || got.Port != 22 || got.User != "deploy" || got.KeyFile != "/k/id" || got.KnownHosts != "/k/kh" {
		t.Errorf("config = %+v", got)
	}
	if got.Passphrase.Reveal() != "pass" || got.Password.Reveal() != "pw" {
		t.Errorf("secrets = %q, %q", got.Passphrase.Reveal(), got.Password.Reveal())
	}
	if got.Getenv == nil {
		t.Error("Getenv not set")
	}

	direct, err := r.SSH(context.Background(), config.Connection{Name: "d", DSN: "redis://x"})
	if err != nil || direct != nil {
		t.Errorf("direct: %+v, %v", direct, err)
	}

	c.SSH.Passphrase = "env:MISSING"
	_, err = r.SSH(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), `connection "prod": ssh.passphrase: resolve`) || strings.Contains(err.Error(), "MISSING") {
		t.Errorf("err = %v, want it to name the connection and field only", err)
	}
}
