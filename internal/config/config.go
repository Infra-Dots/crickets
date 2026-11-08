// Package config loads and validates the crickets ConfigMap.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Infra-Dots/crickets/internal/providers"
	"sigs.k8s.io/yaml"
)

type Prometheus struct {
	URL             string        `json:"url"`
	BearerTokenFile string        `json:"bearerTokenFile"`
	Timeout         time.Duration `json:"-"`
	TimeoutStr      string        `json:"timeout"`
}

type Namespaces struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type Config struct {
	Prometheus Prometheus `json:"prometheus"`
	Window     time.Duration
	MinAge     time.Duration
	Threshold  float64             `json:"threshold"`
	DryRun     bool                `json:"dryRun"`
	Namespaces Namespaces          `json:"namespaces"`
	Selector   string              `json:"selector"`
	Label      string              `json:"label"`
	Providers  []providers.Profile `json:"providers"`

	WindowStr string `json:"window"`
	MinAgeStr string `json:"minAge"`
}

// EvidenceKey is the annotation that carries the evidence, in the label's prefix.
func (c *Config) EvidenceKey() string {
	prefix := "crickets.io/"
	if i := strings.LastIndex(c.Label, "/"); i >= 0 {
		prefix = c.Label[:i+1]
	}
	return prefix + "idle-evidence"
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	// dryRun defaults to true, so it has to be set before unmarshal.
	c := &Config{DryRun: true}
	if err := yaml.UnmarshalStrict(b, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.finish(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) finish() error {
	var err error
	if c.WindowStr == "" {
		c.WindowStr = "168h"
	}
	if c.Window, err = time.ParseDuration(c.WindowStr); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if c.Window < time.Second {
		return fmt.Errorf("window must be at least 1s")
	}
	c.MinAge = c.Window
	if c.MinAgeStr != "" {
		if c.MinAge, err = time.ParseDuration(c.MinAgeStr); err != nil {
			return fmt.Errorf("minAge: %w", err)
		}
		if c.MinAge < 0 {
			return fmt.Errorf("minAge must not be negative")
		}
	}
	if c.Prometheus.URL == "" {
		return fmt.Errorf("prometheus.url is required")
	}
	c.Prometheus.Timeout = 30 * time.Second
	if c.Prometheus.TimeoutStr != "" {
		if c.Prometheus.Timeout, err = time.ParseDuration(c.Prometheus.TimeoutStr); err != nil {
			return fmt.Errorf("prometheus.timeout: %w", err)
		}
	}
	if c.Threshold < 0 {
		return fmt.Errorf("threshold must not be negative")
	}
	if c.Label == "" {
		c.Label = "crickets.io/idle"
	}
	if c.Namespaces.Exclude == nil {
		c.Namespaces.Exclude = []string{"kube-system", "kube-public"}
	}
	return nil
}
