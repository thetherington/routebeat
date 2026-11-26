//go:build !integration
// +build !integration

package config

import (
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/config"
)

func unpack(t *testing.T, raw map[string]any) Config {
	t.Helper()

	cfg, err := config.NewConfigFrom(raw)
	if err != nil {
		t.Fatalf("NewConfigFrom: %v", err)
	}

	c := DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	return c
}

func TestElasticsearchDefaultsWithoutSection(t *testing.T) {
	c := unpack(t, map[string]any{"tags": []string{"MES"}})

	if c.ES.Address != "http://127.0.0.1:9200" {
		t.Errorf("Address = %q, want default", c.ES.Address)
	}
	if c.ES.Index != "log-syslog-informational-*" {
		t.Errorf("Index = %q, want default", c.ES.Index)
	}
	if c.ES.Window != 45*time.Second {
		t.Errorf("Window = %s, want 45s", c.ES.Window)
	}
}

func TestElasticsearchDefaultsKeptWhenNotSet(t *testing.T) {
	c := unpack(t, map[string]any{
		"elasticsearch": map[string]any{"address": "http://127.0.0.1:9200"},
	})

	if c.ES.Address != "http://127.0.0.1:9200" {
		t.Errorf("Address = %q", c.ES.Address)
	}
	if c.ES.Index != "log-syslog-informational-*" {
		t.Errorf("Index = %q, want default", c.ES.Index)
	}
	if c.ES.Delay != time.Minute {
		t.Errorf("Delay = %s, want 1m", c.ES.Delay)
	}
	if c.ES.Window != 45*time.Second {
		t.Errorf("Window = %s, want 45s", c.ES.Window)
	}
}

func TestElasticsearchSettingsOverrideDefaults(t *testing.T) {
	c := unpack(t, map[string]any{
		"elasticsearch": map[string]any{
			"address": "http://es:9200",
			"delay":   "2m",
			"window":  "10s",
		},
	})

	if c.ES.Delay != 2*time.Minute {
		t.Errorf("Delay = %s, want 2m", c.ES.Delay)
	}
	if c.ES.Window != 10*time.Second {
		t.Errorf("Window = %s, want 10s", c.ES.Window)
	}
}

// Unpacking into a copy of DefaultConfig must not change DefaultConfig itself
func TestUnpackDoesNotChangeDefaultConfig(t *testing.T) {
	unpack(t, map[string]any{
		"elasticsearch": map[string]any{"address": "http://es:9200", "window": "10s"},
	})

	if DefaultConfig.ES.Address != "http://127.0.0.1:9200" || DefaultConfig.ES.Window != 45*time.Second {
		t.Errorf("DefaultConfig.ES changed by Unpack: %+v", DefaultConfig.ES)
	}
}
