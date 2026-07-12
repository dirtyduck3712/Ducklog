package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddlewareUsesInboundValidHeader(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := TraceIDFromContext(r.Context())
		seen = id
	}))
	req := httptest.NewRequest("GET", "/", nil)
	valid := "0af76519-16cd-43dd-8448-eb211c80319c"
	req.Header.Set(TraceHeader, valid)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen != valid {
		t.Fatalf("應沿用入站 trace id %q, got %q", valid, seen)
	}
	if rr.Header().Get(TraceHeader) != valid {
		t.Fatal("回應也應帶 trace id")
	}
}

func TestMiddlewareGeneratesWhenMissingOrInvalid(t *testing.T) {
	for _, in := range []string{"", "garbage"} {
		var seen string
		h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, _ = TraceIDFromContext(r.Context())
		}))
		req := httptest.NewRequest("GET", "/", nil)
		if in != "" {
			req.Header.Set(TraceHeader, in)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !isCanonicalUUID(seen) {
			t.Fatalf("入站 %q 時應生成 canonical trace id, got %q", in, seen)
		}
	}
}

func TestInjectTraceID(t *testing.T) {
	ctx := ContextWithTraceID(context.Background(), "abc-123")
	req, _ := http.NewRequest("GET", "http://x", nil)
	InjectTraceID(req, ctx)
	if req.Header.Get(TraceHeader) != "abc-123" {
		t.Fatalf("outbound header = %q; want abc-123", req.Header.Get(TraceHeader))
	}
	// ctx 無 trace id 時不設 header
	req2, _ := http.NewRequest("GET", "http://x", nil)
	InjectTraceID(req2, context.Background())
	if req2.Header.Get(TraceHeader) != "" {
		t.Fatal("無 trace id 不該設 header")
	}
}
