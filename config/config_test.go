//go:build !integration
// +build !integration

package config

import (
	"testing"
	"time"

	"github.com/elastic/elastic-agent-libs/config"
)

func TestWebhookAndEventsAPIDefaults(t *testing.T) {
	c := DefaultConfig

	if err := config.MustNewConfigFrom(map[string]any{}).Unpack(&c); err != nil {
		t.Fatal(err)
	}

	if !c.Webhook.Enabled || c.Webhook.Port != 8082 || c.Webhook.Wait != 5*time.Second {
		t.Errorf("webhook defaults = %+v", c.Webhook)
	}

	if c.EventsAPI.Url != "" || c.EventsAPI.Timeout != 3*time.Second {
		t.Errorf("events api defaults = %+v", c.EventsAPI)
	}
}

func TestWebhookAndEventsAPIUnpack(t *testing.T) {
	c := DefaultConfig

	raw := config.MustNewConfigFrom(map[string]any{
		"webhook": map[string]any{"enabled": false, "wait": "2s"},
		"events_api": map[string]any{
			"url":     "http://host/api?output={output}",
			"headers": map[string]any{"Authorization": "Bearer x"},
		},
	})

	if err := raw.Unpack(&c); err != nil {
		t.Fatal(err)
	}

	if c.Webhook.Enabled || c.Webhook.Port != 8082 || c.Webhook.Wait != 2*time.Second {
		t.Errorf("webhook = %+v", c.Webhook)
	}

	if c.EventsAPI.Url != "http://host/api?output={output}" || c.EventsAPI.Headers["Authorization"] != "Bearer x" {
		t.Errorf("events api = %+v", c.EventsAPI)
	}
}
