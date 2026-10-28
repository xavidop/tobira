package github

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	t.Parallel()
	cloud := func(owner, repo, token string) dsnConfig {
		return dsnConfig{endpoint: cloudEndpoint, graphql: cloudEndpoint + "/graphql", owner: owner, repo: repo, token: token, timeout: defaultTimeout}
	}
	ghe := cloud("octo", "widgets", "")
	ghe.endpoint, ghe.graphql = "https://ghe.example.com/api/v3", "https://ghe.example.com/api/graphql"
	local := cloud("octo", "", "")
	local.endpoint, local.graphql = "http://127.0.0.1:8080", "http://127.0.0.1:8080/graphql"
	lan := cloud("octo", "", "tok")
	lan.endpoint, lan.graphql, lan.plaintextToken = "http://10.0.0.5", "http://10.0.0.5/graphql", true
	lanAnonymous := lan
	lanAnonymous.token, lanAnonymous.plaintextToken = "", false
	slow := cloud("octo", "", "")
	slow.timeout = 5 * time.Second
	fromOptions := ghe
	fromOptions.token = "tok"
	fromOptions.timeout = 2 * time.Minute

	tests := []struct {
		name string
		cfg  driver.ConnConfig
		want dsnConfig
	}{
		{"no owner", driver.ConnConfig{DSN: "github://"}, cloud("", "", "")},
		{"owner", driver.ConnConfig{DSN: "github://octo"}, cloud("octo", "", "")},
		{"owner and repository", driver.ConnConfig{DSN: "github://octo/widgets"}, cloud("octo", "widgets", "")},
		{"trailing slash", driver.ConnConfig{DSN: "github://octo/widgets/"}, cloud("octo", "widgets", "")},
		{"dotted repository", driver.ConnConfig{DSN: "github://octo/widgets.js"}, cloud("octo", "widgets.js", "")},
		{"token in the password position", driver.ConnConfig{DSN: "github://:tok@octo/widgets"}, cloud("octo", "widgets", "tok")},
		{"token without an owner", driver.ConnConfig{DSN: "github://:tok@"}, cloud("", "", "tok")},
		{"password overrides the DSN token", driver.ConnConfig{DSN: "github://:fromdsn@octo", Password: driver.NewSecret("override")}, cloud("octo", "", "override")},
		{"password alone", driver.ConnConfig{DSN: "github://octo", Password: driver.NewSecret("tok")}, cloud("octo", "", "tok")},
		{"enterprise endpoint", driver.ConnConfig{DSN: "github://octo/widgets?endpoint=https://ghe.example.com/api/v3"}, ghe},
		{"enterprise endpoint with a trailing slash", driver.ConnConfig{DSN: "github://octo/widgets?endpoint=https://ghe.example.com/api/v3/"}, ghe},
		{"loopback http needs no opt-in", driver.ConnConfig{DSN: "github://octo?endpoint=http://127.0.0.1:8080"}, local},
		{"lan http with the opt-in", driver.ConnConfig{DSN: "github://:tok@octo?endpoint=http://10.0.0.5&allow_plaintext=1"}, lan},
		{"lan http with the opt-in and no token", driver.ConnConfig{DSN: "github://octo?endpoint=http://10.0.0.5&allow_plaintext=true"}, lanAnonymous},
		{"timeout", driver.ConnConfig{DSN: "github://octo?timeout=5s"}, slow},
		{"upper-case parameter name", driver.ConnConfig{DSN: "github://octo?TIMEOUT=5s"}, slow},
		{"options", driver.ConnConfig{DSN: "github://:tok@octo/widgets", Options: map[string]string{"Endpoint": "https://ghe.example.com/api/v3", "allow_plaintext": "false", "timeout": "2m"}}, fromOptions},
		{"options win over the query", driver.ConnConfig{DSN: "github://:tok@octo/widgets?endpoint=https://other.example.com/api/v3&timeout=1s", Options: map[string]string{"endpoint": "https://ghe.example.com/api/v3", "timeout": "2m"}}, fromOptions},
		{"opt-in without an http endpoint", driver.ConnConfig{DSN: "github://octo?allow_plaintext=1"}, cloud("octo", "", "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDSN(tc.cfg)
			if err != nil {
				t.Fatalf("parseDSN: %v", err)
			}
			if got != tc.want {
				t.Errorf("parseDSN = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseDSNRefusals(t *testing.T) {
	t.Parallel()
	const token = "s3cret-tok"
	tests := []struct {
		name string
		cfg  driver.ConnConfig
		hint string // a fragment of the message
	}{
		{"wrong scheme", driver.ConnConfig{DSN: "https://:" + token + "@github.com"}, "github://"},
		{"no scheme", driver.ConnConfig{DSN: "octo/widgets"}, "github://"},
		{"unparsable", driver.ConnConfig{DSN: "github://:" + token + "@%zz"}, "invalid DSN"},
		{"user name", driver.ConnConfig{DSN: "github://user:" + token + "@octo"}, "no user name"},
		{"port", driver.ConnConfig{DSN: "github://:" + token + "@octo:443"}, "no port"},
		{"owner starting with a dash", driver.ConnConfig{DSN: "github://:" + token + "@-x"}, "not a GitHub user"},
		{"owner of 40 characters", driver.ConnConfig{DSN: "github://" + strings.Repeat("a", 40)}, "not a GitHub user"},
		{"owner with two dashes", driver.ConnConfig{DSN: "github://a--b"}, "not a GitHub user"},
		{"repository with a bad character", driver.ConnConfig{DSN: "github://:" + token + "@octo/wid$gets"}, "not a repository name"},
		{"repository of 101 characters", driver.ConnConfig{DSN: "github://octo/" + strings.Repeat("r", 101)}, "not a repository name"},
		{"two path segments", driver.ConnConfig{DSN: "github://octo/a/b"}, "not a repository name"},
		{"repository without an owner", driver.ConnConfig{DSN: "github:///widgets"}, "needs its owner"},
		{"token parameter", driver.ConnConfig{DSN: "github://octo?token=" + token}, "would be printed"},
		{"password parameter", driver.ConnConfig{DSN: "github://octo?password=" + token}, "would be printed"},
		{"api_key parameter", driver.ConnConfig{DSN: "github://octo?api_key=" + token}, "would be printed"},
		{"access_token parameter", driver.ConnConfig{DSN: "github://octo?access_token=" + token}, "would be printed"},
		{"token option", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"token": token}}, "would be printed"},
		{"unknown parameter", driver.ConnConfig{DSN: "github://:" + token + "@octo?sslmode=disable"}, `unknown option "sslmode"`},
		{"unknown option", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"retries": "3"}}, `unknown option "retries"`},
		{"bad allow_plaintext", driver.ConnConfig{DSN: "github://octo?allow_plaintext=maybe"}, "allow_plaintext must be"},
		{"bad timeout", driver.ConnConfig{DSN: "github://octo?timeout=soon"}, "timeout must be"},
		{"negative timeout", driver.ConnConfig{DSN: "github://octo?timeout=-1s"}, "timeout must be"},
		{"zero timeout", driver.ConnConfig{DSN: "github://octo?timeout=0"}, "timeout must be"},
		{"endpoint without a scheme", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"endpoint": "ghe.example.com/api/v3"}}, "endpoint must be"},
		{"endpoint with another scheme", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"endpoint": "ftp://ghe.example.com"}}, "endpoint must be"},
		{"endpoint with a query", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"endpoint": "https://ghe.example.com/api/v3?x=1"}}, "endpoint must be"},
		{"endpoint with a fragment", driver.ConnConfig{DSN: "github://octo", Options: map[string]string{"endpoint": "https://ghe.example.com/api/v3#x"}}, "endpoint must be"},
		{"endpoint with a user", driver.ConnConfig{DSN: "github://:" + token + "@octo", Options: map[string]string{"endpoint": "https://" + token + "@ghe.example.com/api/v3"}}, "endpoint must be"},
		{"lan http without the opt-in", driver.ConnConfig{DSN: "github://:" + token + "@octo?endpoint=http://10.0.0.5"}, "allow_plaintext=1"},
		{"lan http opted out", driver.ConnConfig{DSN: "github://octo?endpoint=http://10.0.0.5&allow_plaintext=0"}, "allow_plaintext=1"},
		{"http to github.com", driver.ConnConfig{DSN: "github://:" + token + "@octo?endpoint=http://api.github.com&allow_plaintext=1"}, "GitHub's own hosts"},
		{"http to a github.com subdomain", driver.ConnConfig{DSN: "github://octo?endpoint=http://uploads.GitHub.com&allow_plaintext=1"}, "GitHub's own hosts"},
		{"http to githubusercontent.com", driver.ConnConfig{DSN: "github://octo?endpoint=http://raw.githubusercontent.com&allow_plaintext=1"}, "GitHub's own hosts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseDSN(tc.cfg)
			if !errors.Is(err, driver.ErrSyntax) {
				t.Fatalf("parseDSN = %v, want ErrSyntax", err)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name || de.Op != "open" {
				t.Errorf("error attribution = %+v, want github/open", de)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("message %q does not mention %q", err, tc.hint)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("message %q carries the token", err)
			}
		})
	}
}

func TestGraphqlEndpoint(t *testing.T) {
	t.Parallel()
	for rest, want := range map[string]string{
		cloudEndpoint:                    "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3": "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080/graphql",
		"http://127.0.0.1:8080/api/v3":   "http://127.0.0.1:8080/api/graphql",
	} {
		if got := graphqlEndpoint(rest); got != want {
			t.Errorf("graphqlEndpoint(%q) = %q, want %q", rest, got, want)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ns          string
		owner, repo string
		ok          bool
	}{
		{"octo/widgets", "octo", "widgets", true},
		{"Octo-Org/Widgets.js", "Octo-Org", "Widgets.js", true},
		{"o/w_1", "o", "w_1", true},
		{"octo", "", "", false},
		{"octo/", "", "", false},
		{"/widgets", "", "", false},
		{"octo/a/b", "", "", false},
		{"-x/widgets", "", "", false},
		{"a--b/widgets", "", "", false},
		{"octo/wid$gets", "", "", false},
		{"octo/wid gets", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		owner, repo, ok := splitRepo(tc.ns)
		if owner != tc.owner || repo != tc.repo || ok != tc.ok {
			t.Errorf("splitRepo(%q) = %q, %q, %v; want %q, %q, %v", tc.ns, owner, repo, ok, tc.owner, tc.repo, tc.ok)
		}
	}
}
