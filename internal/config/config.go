// Package config 載入 YAML 設定。
package config

import (
	"os"
	"time"

	"docklog/internal/auth"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen             string        `yaml:"listen"`
	DataDir            string        `yaml:"data_dir"`
	CheckpointInterval time.Duration `yaml:"checkpoint_interval"`
	RateLimits         struct {
		Default   float64            `yaml:"default"`
		Overrides map[string]float64 `yaml:"overrides"`
	} `yaml:"rate_limits"`
	APIKeys []auth.Key `yaml:"api_keys"`
}

// UnmarshalYAML 把 checkpoint_interval 的字串("1h")轉成 time.Duration。
// 用 mirror 型別避免 yaml.v3 直接把字串塞進 time.Duration(int64)欄位。
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	type mirror struct {
		Listen             string `yaml:"listen"`
		DataDir            string `yaml:"data_dir"`
		CheckpointInterval string `yaml:"checkpoint_interval"`
		RateLimits         struct {
			Default   float64            `yaml:"default"`
			Overrides map[string]float64 `yaml:"overrides"`
		} `yaml:"rate_limits"`
		APIKeys []auth.Key `yaml:"api_keys"`
	}
	var m mirror
	if err := value.Decode(&m); err != nil {
		return err
	}
	c.Listen = m.Listen
	c.DataDir = m.DataDir
	c.RateLimits = m.RateLimits
	c.APIKeys = m.APIKeys
	if m.CheckpointInterval != "" {
		d, err := time.ParseDuration(m.CheckpointInterval)
		if err != nil {
			return err
		}
		c.CheckpointInterval = d
	}
	return nil
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.CheckpointInterval == 0 {
		c.CheckpointInterval = time.Hour
	}
	// rate_limits.default 漏設 → 0 會讓 token bucket 靜默丟光所有 ingest。
	// 補成 spec 的預設 1000。(overrides 的 0 是合法的「靜音某 service」,不動。)
	if c.RateLimits.Default == 0 {
		c.RateLimits.Default = 1000
	}
	return c, nil
}
