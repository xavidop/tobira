package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sshConfig = `connections:
  - name: prod
    dsn: postgres://app@db.internal:5432/app
    password: env:PROD_PW
    ssh:
      host: bastion.example.com:2222
      user: deploy
      key: ~/.ssh/id_ed25519
      passphrase: env:KEY_PASS
      known_hosts: ~/.ssh/known_hosts
  - name: direct
    dsn: redis://localhost:6379/0
`

func TestSSHBlockLoads(t *testing.T) {
	c, err := Load(writeTemp(t, sshConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := &SSH{Host: "bastion.example.com:2222", User: "deploy", Key: "~/.ssh/id_ed25519",
		Passphrase: "env:KEY_PASS", KnownHosts: "~/.ssh/known_hosts"}
	if got := c.Connections[0].SSH; !reflect.DeepEqual(got, want) {
		t.Errorf("ssh = %+v, want %+v", got, want)
	}
	if c.Connections[1].SSH != nil {
		t.Error("a connection without ssh: has a block")
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings: %v", c.Warnings)
	}
	// A null block means direct.
	c, err = Load(writeTemp(t, "connections:\n  - name: a\n    dsn: redis://x\n    ssh:\n"))
	if err != nil || c.Connections[0].SSH != nil {
		t.Errorf("null ssh: conn=%+v err=%v", c.Connections[0], err)
	}
}

func TestSSHBlockErrors(t *testing.T) {
	conn := func(ssh string) string {
		return "connections:\n  - name: a\n    dsn: redis://x\n    ssh:\n" + ssh
	}
	for name, tt := range map[string]struct{ body, want string }{
		"not a mapping":   {"connections:\n  - name: a\n    dsn: redis://x\n    ssh: bastion\n", "wrong type"},
		"unknown key":     {conn("      host: b\n      knownhosts: /k\n"), `unknown ssh key (did you mean "known_hosts"?)`},
		"no host":         {conn("      user: u\n"), "ssh.host is required"},
		"bad port":        {conn("      host: b\n      port: 70000\n"), "ssh.port must be from 1 to 65535"},
		"relative key":    {conn("      host: b\n      key: keys/id\n"), "ssh.key must be absolute"},
		"relative hosts":  {conn("      host: b\n      known_hosts: kh\n"), "ssh.known_hosts must be absolute"},
		"literal secret":  {conn("      host: b\n      passphrase: hunter2\n"), "ssh.passphrase must be a secret reference"},
		"literal pw":      {conn("      host: b\n      password: hunter2\n"), "ssh.password must be a secret reference"},
		"control in host": {conn("      host: \"b\\u0007\"\n"), "control characters"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeTemp(t, tt.body))
			if err != nil {
				t.Fatalf("the file failed to load as a whole: %v", err)
			}
			if len(c.Broken) != 1 || !strings.Contains(c.Broken[0].Err, tt.want) {
				t.Fatalf("broken = %+v, want one entry containing %q", c.Broken, tt.want)
			}
			if strings.Contains(c.Broken[0].Err, "hunter2") {
				t.Error("the error echoes the secret")
			}
		})
	}
}

func TestSSHBlockSurvivesTheWriter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	st, err := StatOf(p)
	if err != nil {
		t.Fatal(err)
	}
	c := Connection{Name: "prod", DSN: "postgres://app@db.internal:5432/app",
		SSH: &SSH{Host: "bastion", Port: 2222, User: "deploy", Key: "/k/id", Password: "env:SSH_PW", KnownHosts: "/k/known_hosts"}}
	st, err = AddConnection(p, c, st)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Connections[0]; !reflect.DeepEqual(got, c) {
		t.Errorf("after write and load: %+v, want %+v", got, c)
	}
	// An update keeps the block, and dropping it removes it.
	c.SSH.User = "ops"
	if st, err = UpdateConnection(p, "prod", c, st); err != nil {
		t.Fatal(err)
	}
	loaded, _ = Load(p)
	if loaded.Connections[0].SSH.User != "ops" {
		t.Errorf("update lost the ssh change: %+v", loaded.Connections[0].SSH)
	}
	c.SSH = nil
	if _, err = UpdateConnection(p, "prod", c, st); err != nil {
		t.Fatal(err)
	}
	loaded, _ = Load(p)
	if loaded.Connections[0].SSH != nil {
		t.Error("removing the block did not remove it from the file")
	}
	// The writer validates the block as the loader does.
	bad := Connection{Name: "x", DSN: "redis://x", SSH: &SSH{Host: "b", Passphrase: "hunter2"}}
	if err := ValidateConnection(bad, nil); err == nil || !strings.Contains(err.Error(), "ssh.passphrase") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("ValidateConnection = %v", err)
	}
}

func TestSafeFieldsKeepsASafeSSHBlock(t *testing.T) {
	s := &SSH{Host: "b", User: "u\x07", Key: "/k", Passphrase: "hunter2", Password: "env:PW", KnownHosts: "/kh"}
	got := safeFields(Connection{Name: "a", DSN: "redis://x", SSH: s})
	want := &SSH{Host: "b", Key: "/k", Password: "env:PW", KnownHosts: "/kh"}
	if !reflect.DeepEqual(got.SSH, want) {
		t.Errorf("safe ssh = %+v, want %+v", got.SSH, want)
	}
	if safeFields(Connection{SSH: &SSH{Host: " "}}).SSH != nil {
		t.Error("a block with no usable host is kept")
	}
}
