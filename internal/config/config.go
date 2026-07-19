package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Transformers []Transformer `yaml:"transformers"`
}
type Transformer struct {
	Operation  string   `yaml:"operation"`
	Command    []string `yaml:"command"`
	Extensions []string `yaml:"extensions"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	base := filepath.Dir(filepath.Dir(path))
	seen := make(map[string]bool)
	for i := range cfg.Transformers {
		t := &cfg.Transformers[i]
		if t.Operation == "" || len(t.Command) == 0 {
			return Config{}, fmt.Errorf("transformer %d requires operation and command", i+1)
		}
		if seen[t.Operation] {
			return Config{}, fmt.Errorf("duplicate transformer operation %q", t.Operation)
		}
		seen[t.Operation] = true
		// The first command item is the executable. Remaining items are literal
		// arguments, such as Python's "-3" switch and a transformer script path.
		if !filepath.IsAbs(t.Command[0]) {
			t.Command[0] = filepath.Join(base, t.Command[0])
		}
	}
	return cfg, nil
}

func (c Config) Find(operation string) (Transformer, bool) {
	for _, t := range c.Transformers {
		if t.Operation == operation {
			return t, true
		}
	}
	return Transformer{}, false
}

func (t Transformer) Accepts(filename string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	for _, allowed := range t.Extensions {
		if strings.EqualFold(ext, strings.TrimPrefix(allowed, ".")) {
			return true
		}
	}
	return false
}
