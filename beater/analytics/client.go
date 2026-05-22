package analytics

import (
	"context"
	"fmt"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
)

const (
	FROM            = "now-30m"
	PROCESS         = "magrtrsrv"
	REQUEST_LOGS    = "INFO:jsonrpc:Subscribe request"
	COMPLETION_LOGS = "INFO:subscription:Subscription Request Complete"
)

type SearchInterface interface {
	SearchMagnumLogs(source, destination string) ([]Source, error)
	SearchMultiLogs(ctx context.Context, queries ...MultiLogQuery) (MultiLogResults, error)
}

type ClientConfig struct {
	Address string
	Index   string
	Strict  bool
}

type ESSearch struct {
	client  *elasticsearch.TypedClient
	index   string
	request *search.Request
	Strict  bool
}

func StringPtr(s string) *string { return &s }

// New creates a new instance of Elasticsearch
func NewClient(cfg *ClientConfig) (SearchInterface, error) {
	typedClient, err := elasticsearch.NewTypedClient(elasticsearch.Config{
		Addresses: []string{cfg.Address},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// Implementation for creating a new Elasticsearch client goes here
	return &ESSearch{
		client: typedClient,
		index:  cfg.Index,
		Strict: cfg.Strict,
	}, nil
}
