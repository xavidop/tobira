package driver_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

func TestRegistryAdd(t *testing.T) {
	r := driver.NewRegistry()
	if err := r.Add(newFake("pg", "postgres", "PostgreSQL")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		d    driver.Driver
	}{
		{"nil", nil},
		{"empty name", newFake("", "x")},
		{"duplicate name", newFake("pg", "other")},
		{"duplicate scheme", newFake("other", "postgres")},
		{"duplicate scheme case-insensitive", newFake("other2", "POSTGRESQL")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Add(tc.d); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if err := r.Add(newFake("PG", "sqlite")); err != nil {
		t.Fatalf("names are case-sensitive: %v", err)
	}
	if _, ok := r.Lookup("pg"); !ok {
		t.Error("Lookup pg")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("Lookup nope")
	}
	for _, s := range []string{"postgres", "POSTGRESQL", "PostgreSQL"} {
		if d, ok := r.ForScheme(s); !ok || d.Info().Name != "pg" {
			t.Errorf("ForScheme(%q) = %v, %v", s, d, ok)
		}
	}
	if _, ok := r.ForScheme("nope"); ok {
		t.Error("ForScheme nope")
	}
}

func TestRegistryDrivers(t *testing.T) {
	r := driver.NewRegistry()
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if err := r.Add(newFake(n, n+"s")); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, i := range r.Drivers() {
		got = append(got, i.Name)
	}
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRegistryOpen(t *testing.T) {
	ctx := context.Background()
	var gotA, gotB driver.ConnConfig
	a, b := newFake("a", "aa"), newFake("b", "bb")
	a.got, b.got = &gotA, &gotB
	r := driver.NewRegistry()
	_ = r.Add(a)
	_ = r.Add(b)

	tests := []struct {
		name    string
		cfg     driver.ConnConfig
		want    string // driver expected to receive cfg
		unknown bool
	}{
		{"explicit driver wins over scheme", driver.ConnConfig{Driver: "a", DSN: "bb://h/db"}, "a", false},
		{"scheme selects", driver.ConnConfig{DSN: "BB://h/db"}, "b", false},
		{"unknown scheme", driver.ConnConfig{DSN: "zz://h/db"}, "", true},
		{"unknown explicit driver", driver.ConnConfig{Driver: "zz", DSN: "aa://h"}, "", true},
		{"neither", driver.ConnConfig{}, "", true},
		{"no scheme", driver.ConnConfig{DSN: "./x.db"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotA, gotB = driver.ConnConfig{}, driver.ConnConfig{}
			tc.cfg.Options = map[string]string{"k": "v"}
			c, err := r.Open(ctx, tc.cfg)
			if tc.unknown {
				if !errors.Is(err, driver.ErrUnknownDriver) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || c == nil {
				t.Fatalf("Open: %v", err)
			}
			got, other := gotA, gotB
			if tc.want == "b" {
				got, other = gotB, gotA
			}
			if !reflect.DeepEqual(got, tc.cfg) {
				t.Errorf("cfg changed: %+v vs %+v", got, tc.cfg)
			}
			if !reflect.DeepEqual(other, driver.ConnConfig{}) {
				t.Errorf("wrong driver opened")
			}
		})
	}
}

func TestRegistryOpenErrorRedactsDSN(t *testing.T) {
	r := driver.NewRegistry()
	_, err := r.Open(context.Background(), driver.ConnConfig{DSN: "zz://u:hunter2@h/db"})
	if !errors.Is(err, driver.ErrUnknownDriver) {
		t.Fatal(err)
	}
	if s := err.Error(); strings.Contains(s, "hunter2") || !strings.Contains(s, "zz") {
		t.Errorf("message %q", s)
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	defer driver.SwapDefault()()
	driver.Register(newFake("dup", "dups"))
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	driver.Register(newFake("dup", "other"))
}

func TestDefaultHelpers(t *testing.T) {
	defer driver.SwapDefault()()
	driver.Register(newFake("one", "ones"))
	ctx := context.Background()
	if c, err := driver.Open(ctx, "ones://x"); err != nil || c == nil {
		t.Fatalf("Open: %v", err)
	}
	if c, err := driver.OpenConfig(ctx, driver.ConnConfig{Driver: "one"}); err != nil || c == nil {
		t.Fatalf("OpenConfig: %v", err)
	}
	if ds := driver.Drivers(); len(ds) != 1 || ds[0].Name != "one" {
		t.Fatalf("Drivers = %v", ds)
	}
}
