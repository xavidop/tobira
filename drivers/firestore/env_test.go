package firestore

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
)

// FIRESTORE_EMULATOR_HOST makes the client library send every RPC to that
// host. A DSN without emulator= is meant for Google, so Open refuses
// rather than be retargeted; with emulator= the two must agree.
func TestEmulatorHostEnvRefused(t *testing.T) {
	t.Setenv(emulatorEnv, "127.0.0.1:1")
	for _, dsn := range []string{"firestore://prod-project", "firestore://p?emulator=127.0.0.1:2"} {
		err := openErr(t, driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), emulatorEnv) {
			t.Errorf("Open(%s) with %s set: want ErrUnsupported naming the variable, got %v", dsn, emulatorEnv, err)
		}
	}
}

// The variable is read exactly as the client library reads it: a value of
// only spaces still retargets the library, and a padded address is not the
// DSN's, so both are refused instead of being trimmed away.
func TestEmulatorHostEnvNotTrimmed(t *testing.T) {
	for env, dsn := range map[string]string{
		" ":            "firestore://prod-project",
		"\t":           "firestore://prod-project",
		"http://":      "firestore://prod-project",
		" 127.0.0.1:1": "firestore://p?emulator=127.0.0.1:1",
		"127.0.0.1:1 ": "firestore://p?emulator=127.0.0.1:1",
	} {
		t.Setenv(emulatorEnv, env)
		err := openErr(t, driver.ConnConfig{DSN: dsn})
		if !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), emulatorEnv) {
			t.Errorf("env %q, Open(%s): want ErrUnsupported naming the variable, got %v", env, dsn, err)
		}
	}
}

// When the variable and emulator= agree, Open goes ahead, and a failed
// open leaks no connection (goleak checks the package at exit).
func TestEmulatorHostEnvAgrees(t *testing.T) {
	defer func(d time.Duration) { openTimeout = d }(openTimeout)
	openTimeout = time.Second
	for _, env := range []string{"127.0.0.1:1", "http://127.0.0.1:1"} {
		t.Setenv(emulatorEnv, env)
		err := openErr(t, driver.ConnConfig{DSN: "firestore://p?emulator=127.0.0.1:1"})
		if !errors.Is(err, driver.ErrConnection) {
			t.Errorf("env %q: want ErrConnection from a closed port, got %v", env, err)
		}
	}
}

// fakeServer answers every RPC with code.
func fakeServer(t *testing.T, code codes.Code) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
		return status.Error(code, "request had invalid authentication credentials")
	}))
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return l.Addr().String()
}

// A server that rejects the caller's credentials is ErrAuth from Open,
// through the real client library; a refused permission is ErrPermission.
func TestServerRejectsCredentials(t *testing.T) {
	for code, want := range map[codes.Code]error{codes.Unauthenticated: driver.ErrAuth, codes.PermissionDenied: driver.ErrPermission} {
		addr := fakeServer(t, code)
		err := openErr(t, driver.ConnConfig{DSN: "firestore://p?emulator=" + addr})
		if !errors.Is(err, want) {
			t.Errorf("%v: want %v, got %v", code, want, err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Op != "open" || de.Code != code.String() {
			t.Errorf("%v: error = %#v", code, err)
		}
	}
}
