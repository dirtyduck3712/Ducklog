package model

import "testing"

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{"debug": 0, "info": 1, "warn": 2, "error": 3}
	for name, want := range cases {
		got, ok := ParseLevel(name)
		if !ok || got != want {
			t.Fatalf("ParseLevel(%q) = %d,%v; want %d,true", name, got, ok, want)
		}
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Fatal("ParseLevel(bogus) should return ok=false")
	}
}

func TestParseLevelCaseInsensitive(t *testing.T) {
	cases := map[string]Level{"ERROR": Error, "Warn": Warn, "INFO": Info}
	for name, want := range cases {
		got, ok := ParseLevel(name)
		if !ok || got != want {
			t.Fatalf("ParseLevel(%q) = %d,%v; want %d,true", name, got, ok, want)
		}
	}
}

func TestLevelString(t *testing.T) {
	if Level(3).String() != "error" {
		t.Fatalf("Level(3).String() = %q; want error", Level(3).String())
	}
}
