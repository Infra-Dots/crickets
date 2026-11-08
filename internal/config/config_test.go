package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
		check   func(*testing.T, *Config)
	}{
		{"defaults", "prometheus: {url: http://p:9090}", "", func(t *testing.T, c *Config) {
			if c.Window != 168*time.Hour || c.MinAge != c.Window || !c.DryRun || c.Label != "crickets.io/idle" ||
				c.Prometheus.Timeout != 30*time.Second || len(c.Namespaces.Exclude) != 2 {
				t.Fatalf("bad defaults: %+v", c)
			}
		}},
		{"overrides", "prometheus: {url: http://p, timeout: 5s}\nwindow: 2m\nminAge: 0s\ndryRun: false\nthreshold: 3\nlabel: acme.io/idle\nnamespaces: {exclude: []}", "", func(t *testing.T, c *Config) {
			if c.Window != 2*time.Minute || c.MinAge != 0 || c.DryRun || c.Threshold != 3 || c.Prometheus.Timeout != 5*time.Second {
				t.Fatalf("bad: %+v", c)
			}
			if c.EvidenceKey() != "acme.io/idle-evidence" {
				t.Fatal(c.EvidenceKey())
			}
		}},
		{"no url", "window: 1h", "prometheus.url", nil},
		{"bad window", "prometheus: {url: x}\nwindow: soon", "window", nil},
		{"tiny window", "prometheus: {url: x}\nwindow: 10ms", "at least 1s", nil},
		{"bad minAge", "prometheus: {url: x}\nminAge: -1h", "minAge", nil},
		{"negative threshold", "prometheus: {url: x}\nthreshold: -1", "threshold", nil},
		{"unknown key", "prometheus: {url: x}\nbogus: 1", "bogus", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}
