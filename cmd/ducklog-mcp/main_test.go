package main

import "testing"

func TestResolveVLURL(t *testing.T) {
	t.Run("env override", func(t *testing.T) {
		t.Setenv("VL_URL", "http://vl.internal:9428")
		if got := resolveVLURL(); got != "http://vl.internal:9428" {
			t.Fatalf("resolveVLURL() = %q, want env value", got)
		}
	})
	t.Run("default fallback", func(t *testing.T) {
		t.Setenv("VL_URL", "")
		if got := resolveVLURL(); got != defaultVLURL {
			t.Fatalf("resolveVLURL() = %q, want %q", got, defaultVLURL)
		}
	})
}
