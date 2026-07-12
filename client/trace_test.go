package client

import (
	"context"
	"testing"
)

func TestNewTraceIDIsCanonical(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := NewTraceID()
		if !isCanonicalUUID(id) {
			t.Fatalf("NewTraceID 產生非 canonical UUID: %q", id)
		}
	}
	// 兩次不應相同
	if NewTraceID() == NewTraceID() {
		t.Fatal("NewTraceID 不該重複")
	}
}

func TestIsCanonicalUUID(t *testing.T) {
	good := "0af76519-16cd-43dd-8448-eb211c80319c"
	bad := []string{"", "not-a-uuid", "0af7651916cd43dd8448eb211c80319c",
		"{0af76519-16cd-43dd-8448-eb211c80319c}", "0af76519-16cd-43dd-8448-eb211c80319"}
	if !isCanonicalUUID(good) {
		t.Fatalf("%q 應為 canonical", good)
	}
	for _, b := range bad {
		if isCanonicalUUID(b) {
			t.Fatalf("%q 不該被當 canonical", b)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "abc")
	if id, ok := TraceIDFromContext(ctx); !ok || id != "abc" {
		t.Fatalf("round trip = %q,%v; want abc,true", id, ok)
	}
	if _, ok := TraceIDFromContext(context.Background()); ok {
		t.Fatal("空 context 不應有 trace id")
	}
}
