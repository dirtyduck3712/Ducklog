package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`
listen: ":8080"
data_dir: "data"
checkpoint_interval: "1h"
rate_limits:
  default: 1000
  overrides:
    batch-worker: 5000
api_keys:
  - key: "ingest-secret"
    name: "k8s"
    scopes: ["ingest"]
`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.RateLimits.Default != 1000 ||
		c.RateLimits.Overrides["batch-worker"] != 5000 ||
		len(c.APIKeys) != 1 || c.APIKeys[0].Scopes[0] != "ingest" {
		t.Fatalf("config 解析錯誤:%+v", c)
	}
	if c.CheckpointInterval.String() != "1h0m0s" {
		t.Fatalf("interval 解析錯誤:%v", c.CheckpointInterval)
	}
}
