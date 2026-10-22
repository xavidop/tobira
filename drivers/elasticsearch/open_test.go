package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestParseDSN(t *testing.T) {
	for _, tc := range []struct {
		dsn     string
		opts    map[string]string
		pw      string
		fl      flavor
		want    string // address
		user    string
		pass    string
		apiKey  string
		ca      string
		skip    bool
		plainPw bool
	}{
		{dsn: "elasticsearch://localhost", want: "https://localhost:9200"},
		{dsn: "elasticsearch://es.internal:9243", want: "https://es.internal:9243"},
		{dsn: "elasticsearch://elastic:s3cret@es.internal", want: "https://es.internal:9200", user: "elastic", pass: "s3cret"},
		{dsn: "elasticsearch://:a2V5aWQ6a2V5@es.internal", want: "https://es.internal:9200", apiKey: "a2V5aWQ6a2V5"},
		{dsn: "elasticsearch://elastic@es.internal", pw: "from-ref", want: "https://es.internal:9200", user: "elastic", pass: "from-ref"},
		{dsn: "elasticsearch://es.internal", pw: "keyid:keysecret", want: "https://es.internal:9200", apiKey: "a2V5aWQ6a2V5c2VjcmV0"},
		{dsn: "elasticsearch://localhost:9200?tls=false", want: "http://localhost:9200"},
		{dsn: "elasticsearch://elastic:pw@es.internal?tls=false&allow_plaintext=1", want: "http://es.internal:9200", user: "elastic", pass: "pw", plainPw: true},
		{dsn: "elasticsearch://es.internal", opts: map[string]string{"tls": "false", "allow_plaintext": "true"}, want: "http://es.internal:9200"},
		{dsn: "elasticsearch://es.internal?allow_plaintext=1", want: "https://es.internal:9200"},
		{dsn: "elasticsearch://elastic:pw@127.0.0.1?tls=false", want: "http://127.0.0.1:9200", user: "elastic", pass: "pw"},
		{dsn: "elasticsearch://localhost?ca_file=/tmp/ca.pem", want: "https://localhost:9200", ca: "/tmp/ca.pem"},
		{dsn: "elasticsearch://localhost", opts: map[string]string{"tls_skip_verify": "true"}, want: "https://localhost:9200", skip: true},
		{dsn: "elasticsearch://[::1]:9201", want: "https://[::1]:9201"},
		{dsn: "opensearch://localhost?tls=false", fl: flavorOpenSearch, want: "http://localhost:9200"},
	} {
		t.Run(tc.dsn, func(t *testing.T) {
			cfg := driver.ConnConfig{DSN: tc.dsn, Options: tc.opts}
			if tc.pw != "" {
				cfg.Password = driver.NewSecret(tc.pw)
			}
			d, err := parseDSN(cfg, tc.fl)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.address(); got != tc.want {
				t.Errorf("address = %q, want %q", got, tc.want)
			}
			if d.user != tc.user || d.password != tc.pass || d.apiKey != tc.apiKey {
				t.Errorf("credentials = (%q, %q, key %q), want (%q, %q, key %q)", d.user, d.password, d.apiKey, tc.user, tc.pass, tc.apiKey)
			}
			if d.caFile != tc.ca || d.skipVerify != tc.skip || d.plaintextCred != tc.plainPw {
				t.Errorf("ca %q skip %v plaintext %v, want %q %v %v", d.caFile, d.skipVerify, d.plaintextCred, tc.ca, tc.skip, tc.plainPw)
			}
		})
	}
}

func TestParseDSNRefusals(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		fl   flavor
		opts map[string]string
	}{
		{dsn: "postgres://localhost"},
		{dsn: "opensearch://localhost"}, // the elasticsearch driver takes elasticsearch://
		{dsn: "elasticsearch://localhost", fl: flavorOpenSearch},
		{dsn: "elasticsearch://localhost:0"},
		{dsn: "elasticsearch://localhost:99999"},
		{dsn: "elasticsearch://localhost/prefix"},
		{dsn: "elasticsearch://localhost?api_key=s3cr3t-value"},
		{dsn: "elasticsearch://localhost?password=s3cr3t-value"},
		{dsn: "elasticsearch://localhost?tls=maybe"},
		{dsn: "elasticsearch://localhost?bogus=1"},
		{dsn: "elasticsearch://localhost", opts: map[string]string{"bogus": "1"}},
		{dsn: "elasticsearch://localhost?tls=false&ca_file=/x"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn, Options: tc.opts}, tc.fl)
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("parseDSN(%q): want ErrSyntax, got %v", tc.dsn, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t-value") {
			t.Errorf("parseDSN(%q): the error echoes a secret: %v", tc.dsn, err)
		}
	}
}

// Open errors never carry the password or the API key, whatever the cause.
func TestOpenErrorsAreRedacted(t *testing.T) {
	for _, dsn := range []string{
		"elasticsearch://elastic:hunter2-pw@127.0.0.1:1?tls=false",
		"elasticsearch://:hunter2-pw@127.0.0.1:1?tls=false",
		"elasticsearch://elastic:hunter2-pw@127.0.0.1:1?ca_file=/no/such/hunter2-pw.pem",
	} {
		c, err := New().Open(context.Background(), driver.ConnConfig{DSN: dsn})
		if err == nil {
			_ = c.Close()
			t.Fatalf("Open(%s) succeeded on a closed port", driver.RedactDSN(dsn))
		}
		if strings.Contains(err.Error(), "hunter2-pw") {
			t.Errorf("Open error carries the secret: %v", err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Driver != Name {
			t.Errorf("Open error is not a driver.Error of %q: %#v", Name, err)
		}
	}
}

func TestInfo(t *testing.T) {
	es, os := New().Info(), NewOpenSearch().Info()
	if es.Name != "elasticsearch" || es.Kind != driver.KindSearch || es.Schemes[0] != "elasticsearch" {
		t.Errorf("elasticsearch Info = %+v", es)
	}
	if os.Name != "opensearch" || os.Kind != driver.KindSearch || os.Schemes[0] != "opensearch" {
		t.Errorf("opensearch Info = %+v", os)
	}
	if !driver.HasCapability(es, driver.CapVectorSearcher) || driver.HasCapability(os, driver.CapVectorSearcher) {
		t.Error("only the elasticsearch driver offers vector search")
	}
	for _, info := range []driver.Info{es, os} {
		if !driver.HasCapability(info, driver.CapReadOnly) {
			t.Errorf("%s does not declare readonly", info.Name)
		}
	}
	osConn := &conn{fl: flavorOpenSearch}
	if _, ok := driver.As[driver.VectorSearcher](osConn); ok {
		t.Error("an opensearch conn offers VectorSearcher")
	}
	if _, ok := driver.As[driver.VectorSearcher](&conn{fl: flavorElastic}); !ok {
		t.Error("an elasticsearch conn does not offer VectorSearcher")
	}
}

// Plain http to a host that is not loopback needs allow_plaintext=1, and the
// managed clouds are never reached in clear.
func TestParseDSNPlaintextRule(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		fl   flavor
		want string
	}{
		{dsn: "elasticsearch://es.internal?tls=false", want: "allow_plaintext=1"},
		{dsn: "elasticsearch://elastic:pw@es.internal?tls=false&allow_plaintext=0", want: "allow_plaintext=1"},
		{dsn: "opensearch://os.internal?tls=false", fl: flavorOpenSearch, want: "allow_plaintext=1"},
		{dsn: "elasticsearch://es.internal?allow_plaintext=maybe", want: "allow_plaintext must be"},
		{dsn: "elasticsearch://my.es.us-east-1.aws.elastic-cloud.com?tls=false&allow_plaintext=1", want: "managed"},
		{dsn: "elasticsearch://My.ES.Europe-West1.GCP.Cloud.ES.IO.?tls=false&allow_plaintext=1", want: "managed"},
		{dsn: "opensearch://search-x.us-east-1.es.amazonaws.com?tls=false&allow_plaintext=1", fl: flavorOpenSearch, want: "managed"},
		{dsn: "opensearch://abc.us-east-1.aoss.amazonaws.com?tls=false&allow_plaintext=1", fl: flavorOpenSearch, want: "managed"},
		{dsn: "opensearch://search-x.cn-north-1.es.amazonaws.com.cn?tls=false&allow_plaintext=1", fl: flavorOpenSearch, want: "managed"},
		{dsn: "elasticsearch://my-project.es.us-east-1.aws.elastic.cloud?tls=false&allow_plaintext=1", want: "managed"},
	} {
		_, err := parseDSN(driver.ConnConfig{DSN: tc.dsn}, tc.fl)
		if !errors.Is(err, driver.ErrSyntax) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseDSN(%q) = %v, want ErrSyntax containing %q", tc.dsn, err, tc.want)
		}
	}
}

// The transport never follows a redirect: the credentials must never reach
// the second server. (elastictransport calls RoundTrip directly, with no
// http.Client, so this pins that it stays that way.)
func TestRedirectIsNotFollowed(t *testing.T) {
	var hits, authed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			authed.Add(1)
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":{"number":"8.15.0"},"tagline":"You Know, for Search"}`))
	}))
	t.Cleanup(target.Close)
	// Same host name, another port: Go would keep Authorization there.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	host := strings.TrimPrefix(redirector.URL, "http://")
	for _, tc := range []struct {
		fl  flavor
		dsn string
	}{
		{flavorElastic, "elasticsearch://elastic:s3cr3t-pw@" + host + "?tls=false"},
		{flavorElastic, "elasticsearch://:a2V5aWQ6czNjcjN0@" + host + "?tls=false"},
		{flavorOpenSearch, "opensearch://admin:s3cr3t-pw@" + host + "?tls=false"},
	} {
		c, err := esDriver{fl: tc.fl}.Open(context.Background(), driver.ConnConfig{DSN: tc.dsn})
		if err == nil {
			_ = c.Close()
			t.Errorf("Open(%s) through a redirect succeeded", driver.RedactDSN(tc.dsn))
		}
	}
	if n := authed.Load(); n != 0 {
		t.Errorf("the redirect target got credentials %d times", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the redirect target got %d requests", n)
	}
}

func TestAllowSystemIndicesDSN(t *testing.T) {
	d, err := parseDSN(driver.ConnConfig{DSN: "elasticsearch://localhost?allow_system_indices=1"}, flavorElastic)
	if err != nil || !d.allowSystem {
		t.Errorf("allow_system_indices=1: %+v, %v", d, err)
	}
	d, err = parseDSN(driver.ConnConfig{DSN: "elasticsearch://localhost"}, flavorElastic)
	if err != nil || d.allowSystem {
		t.Errorf("default: %+v, %v", d, err)
	}
	if _, err := parseDSN(driver.ConnConfig{DSN: "elasticsearch://localhost?allow_system_indices=maybe"}, flavorElastic); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("a bad value: want ErrSyntax, got %v", err)
	}
}
