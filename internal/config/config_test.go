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

// 漏設 rate_limits.default 時,limiter rate 會是 0 → token bucket 靜默丟光所有 ingest。
// Load 必須把它補成 spec 的預設 1000,避免這個 footgun。
func TestRateLimitDefaultApplied(t *testing.T) {
	write := func(body string) Config {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte(body), 0o644)
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// 整個 rate_limits 區塊省略 → default 補 1000
	if c := write("data_dir: data\n"); c.RateLimits.Default != 1000 {
		t.Fatalf("省略 rate_limits 時 default = %v; want 1000", c.RateLimits.Default)
	}

	// 只給 overrides、漏 default → default 補 1000,override 保留
	c := write("rate_limits:\n  overrides:\n    batch-worker: 5000\n")
	if c.RateLimits.Default != 1000 {
		t.Fatalf("漏 default 時 = %v; want 1000", c.RateLimits.Default)
	}
	if c.RateLimits.Overrides["batch-worker"] != 5000 {
		t.Fatalf("override 應保留:%v", c.RateLimits.Overrides)
	}

	// 明確設定的 default 不被覆蓋
	if c := write("rate_limits:\n  default: 500\n"); c.RateLimits.Default != 500 {
		t.Fatalf("明確 default 被覆蓋:%v", c.RateLimits.Default)
	}

	// override 的 0 是合法的「靜音某 service」,不該被動到
	c2 := write("rate_limits:\n  default: 1000\n  overrides:\n    noisy: 0\n")
	if v, ok := c2.RateLimits.Overrides["noisy"]; !ok || v != 0 {
		t.Fatalf("override 的 0 應保留為 mute:%v", c2.RateLimits.Overrides)
	}
}
