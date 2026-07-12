// Package auth 做純 Bearer token → scopes 對應。
package auth

import "strings"

type Key struct {
	Key    string   `yaml:"key"`
	Name   string   `yaml:"name"`
	Scopes []string `yaml:"scopes"`
}

func (k *Key) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

type KeyStore struct{ byKey map[string]*Key }

func New(keys []Key) *KeyStore {
	m := make(map[string]*Key, len(keys))
	for i := range keys {
		k := keys[i]
		m[k.Key] = &k
	}
	return &KeyStore{byKey: m}
}

// Authenticate 接受完整的 Authorization header 值(含 "Bearer " 前綴)。
func (ks *KeyStore) Authenticate(authHeader string) (*Key, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return nil, false
	}
	k, ok := ks.byKey[strings.TrimPrefix(authHeader, prefix)]
	return k, ok
}
