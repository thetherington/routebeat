package analytics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
)

type BusRouteMap = map[string]*BusRouting

type SearchInterface interface {
	// QuerySchedulerEventParams queries the Elasticsearch index for bus routing information based on the provided relative time.
	QuerySchedulerEventParams(ctx context.Context, opt ...string) (BusRouteMap, error)
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

	// optional credentials. APIKey takes precedence over Username/Password when both are set
	Username string
	Password string
	APIKey   string

	// optional TLS settings for https addresses
	CACert             string // path to a PEM encoded CA certificate, appended to the system pool
	InsecureSkipVerify bool
}

// New creates a new instance of Elasticsearch
func NewClient(cfg *ClientConfig) (SearchInterface, error) {
	transport, err := newTransport(cfg)
	if err != nil {
		return nil, err
	}

	esCfg := elasticsearch.Config{
		Addresses: []string{cfg.Address},
		Transport: transport,
	}

	// only apply credentials when provided, otherwise connect anonymously
	switch {
	case cfg.APIKey != "":
		esCfg.APIKey = cfg.APIKey

	case cfg.Username != "" || cfg.Password != "":
		if cfg.Username == "" || cfg.Password == "" {
			return nil, ErrIncompleteCredentials
		}

		esCfg.Username = cfg.Username
		esCfg.Password = cfg.Password
	}

	typedClient, err := elasticsearch.NewTypedClient(esCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// pre-create request with query and aggregations
	req := search.NewRequest()

	// bool query
	req.Query = createQuery("now")

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

// newTransport builds a dedicated transport so the TLS settings are explicit rather than
// inherited from http.DefaultTransport, which other packages modify globally
func newTransport(cfg *ClientConfig) (*http.Transport, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read ca certificate: %w", err)
		}

		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}

		if !pool.AppendCertsFromPEM(pem) {
			return nil, ErrInvalidCACert
		}

		tlsCfg.RootCAs = pool
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg

	return transport, nil
}

// QuerySchedulerEventParams queries the Elasticsearch index for bus routing information based on the provided relative time.
func (es *ESSearch) QuerySchedulerEventParams(ctx context.Context, opt ...string) (BusRouteMap, error) {
	relative_time := "now"
	if len(opt) > 0 {
		relative_time = opt[0]
	}

	// update query with relative time
	es.request.Query = createQuery(relative_time)

	// send query
	resp, err := es.client.Search().Index(es.index).Request(es.request).Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query elasticsearch: %w", err)
	}

	// total can be omitted from the response, in which case the bucket checks below handle empty results
	if resp.Hits.Total != nil && resp.Hits.Total.Value == 0 {
		return nil, ErrNoHitsReturned
	}

	// fmt.Printf("%+v\n", resp)

	// "aggregations": { "bus_name": { "doc_count_error_upper_bound": 0, "sum_other_doc_count": 0, "buckets": [] }}

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
