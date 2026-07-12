package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
)

// NewTraceID 用 crypto/rand 產生 canonical 的 UUIDv4 字串。
func NewTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var canonicalUUIDRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isCanonicalUUID 檢查是否為 8-4-4-4-12 十六進位格式。
func isCanonicalUUID(s string) bool { return canonicalUUIDRe.MatchString(s) }

type traceIDKey struct{}

func ContextWithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

func TraceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(traceIDKey{}).(string)
	return id, ok && id != ""
}
