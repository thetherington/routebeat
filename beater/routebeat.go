package beater

import (
	"fmt"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"

	routeCfg "github.com/thetherington/routebeat/config"
)

// routebeat configuration.
type routebeat struct {
	done   chan struct{}
	config routeCfg.Config
	client beat.Client
}

// New creates an instance of routebeat.
func New(b *beat.Beat, cfg *config.C) (beat.Beater, error) {
	c := routeCfg.DefaultConfig
	if err := cfg.Unpack(&c); err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	bt := &routebeat{
		done:   make(chan struct{}),
		config: c,
	}
	return bt, nil
}

// Run starts routebeat.
func (bt *routebeat) Run(b *beat.Beat) error {
	logp.Info("routebeat is running! Hit CTRL-C to stop it.")

	var err error
	bt.client, err = b.Publisher.Connect()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(bt.config.Period)
	counter := 1
	for {
		select {
		case <-bt.done:
			return nil
		case <-ticker.C:
		}

		event := beat.Event{
			Timestamp: time.Now(),
			Fields: mapstr.M{
				"type":    b.Info.Name,
				"counter": counter,
			},
		}

		bt.client.Publish(event)
		logp.Info("Event sent")
		counter++
	}
}

// Stop stops routebeat.
func (bt *routebeat) Stop() {
	bt.client.Close()
	close(bt.done)
}
