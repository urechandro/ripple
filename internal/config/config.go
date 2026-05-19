package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Skip      *string  `yaml:"skip"`
	Depth     *int     `yaml:"depth"`
	Method    *string  `yaml:"method"`
	TestFlags []string `yaml:"test_flags"`
}

func Load(root string) (*Config, error) {
	path := filepath.Join(root, ".ripple.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
