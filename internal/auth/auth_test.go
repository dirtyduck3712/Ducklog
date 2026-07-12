package auth

import "testing"

func store() *KeyStore {
	return New([]Key{
		{Key: "ingest-secret", Name: "k8s", Scopes: []string{"ingest"}},
		{Key: "query-secret", Name: "claude", Scopes: []string{"query", "mcp"}},
	})
}

func TestAuthenticate(t *testing.T) {
	k, ok := store().Authenticate("Bearer ingest-secret")
	if !ok || k.Name != "k8s" {
		t.Fatalf("Authenticate ingest = %+v, %v", k, ok)
	}
	if !k.HasScope("ingest") || k.HasScope("query") {
		t.Fatal("scope 判斷錯誤")
	}
}

func TestRejectBadKey(t *testing.T) {
	if _, ok := store().Authenticate("Bearer nope"); ok {
		t.Fatal("未知 key 應被拒")
	}
	if _, ok := store().Authenticate("ingest-secret"); ok {
		t.Fatal("缺 Bearer 前綴應被拒")
	}
}
