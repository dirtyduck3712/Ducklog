package client

import (
	"context"
	"net/http"
)

const TraceHeader = "X-Trace-Id"

// Middleware 讀入站 X-Trace-Id(不合法或缺少就生成 canonical UUID),
// 塞進 request context,並在回應帶上同一個 id 方便除錯。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(TraceHeader)
		if !isCanonicalUUID(id) {
			id = NewTraceID()
		}
		w.Header().Set(TraceHeader, id)
		next.ServeHTTP(w, r.WithContext(ContextWithTraceID(r.Context(), id)))
	})
}

// InjectTraceID 把 ctx 裡的 trace id 設到 outbound 請求,讓 trace 跨服務延續。
func InjectTraceID(req *http.Request, ctx context.Context) {
	if id, ok := TraceIDFromContext(ctx); ok {
		req.Header.Set(TraceHeader, id)
	}
}
