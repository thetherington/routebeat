package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
)

type BusRouteMap = map[string]*BusRouting

type SearchInterface interface {
	QuerySchedulerEventParams(context.Context) (BusRouteMap, error)
}

type ESSearch struct {
	client  *elasticsearch.TypedClient
	index   string
	request *search.Request
}

type BusRouting struct {
	Pri       string
	Sec       string
	StartDate *time.Time
	EndDate   *time.Time
}

type ClientConfig struct {
	Address string
	Index   string
}

// New creates a new instance of Elasticsearch
func NewClient(cfg *ClientConfig) (SearchInterface, error) {
	typedClient, err := elasticsearch.NewTypedClient(elasticsearch.Config{
		Addresses: []string{cfg.Address},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// pre-create request with query and aggregations
	req := search.NewRequest()

	// bool query
	req.Query = createQuery()

	// Root: Terms, Sub: Top Metrics aggregations
	req.Aggregations = createAggregations()

	// 0 size
	req.Size = esapi.IntPtr(0)

	return &ESSearch{
		client:  typedClient,
		index:   cfg.Index,
		request: req,
	}, nil
}

func (es *ESSearch) QuerySchedulerEventParams(ctx context.Context) (BusRouteMap, error) {
	// send query
	resp, err := es.client.Search().Index(es.index).Request(es.request).Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query elasticsearch: %w", err)
	}

	if resp.Hits.Total.Value == 0 {
		return nil, ErrNoHitsReturned
	}

	// fmt.Printf("%+v\n", resp)

	// Cast the "bus_name" aggregation response into the StringTermsAggregate
	stringTermsAgg, ok := resp.Aggregations["bus_name"].(*types.StringTermsAggregate)
	if !ok {
		return nil, ErrCastAggResponse
	}

	// Cast the buckets into a slice of StringTermsBuckets
	buckets, ok := stringTermsAgg.Buckets.([]types.StringTermsBucket)
	if !ok {
		return nil, ErrCastStringTermsBucket
	}

	if len(buckets) == 0 {
		return nil, ErrZeroBuckets
	}

	return processBucketsIntoBusMap(buckets), nil
}
