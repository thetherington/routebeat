package analytics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

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
	Address               string
	Index                 string
	Strict                bool
	UseLogIngestTimestamp bool
	TimeZoneFix           bool

	// optional credentials. APIKey takes precedence over Username/Password when both are set
	Username string
	Password string
	APIKey   string

	// optional TLS settings for https addresses
	CACert             string // path to a PEM encoded CA certificate, appended to the system pool
	InsecureSkipVerify bool
}

type ESSearch struct {
	client                *elasticsearch.TypedClient
	index                 string
	request               *search.Request
	Strict                bool
	UseLogIngestTimestamp bool
	TimeZoneFix           bool
}

func StringPtr(s string) *string { return &s }

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

	// Implementation for creating a new Elasticsearch client goes here
	return &ESSearch{
		client:                typedClient,
		index:                 cfg.Index,
		Strict:                cfg.Strict,
		UseLogIngestTimestamp: cfg.UseLogIngestTimestamp,
		TimeZoneFix:           cfg.TimeZoneFix,
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
