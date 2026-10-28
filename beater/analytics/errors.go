package analytics

import "errors"

var (
	ErrNoHitsReturned          = errors.New("no hits returned in query")
	ErrCastAggResponse         = errors.New("failed casting response into *types.StringTermsAggregate")
	ErrCastStringTermsBucket   = errors.New("failed casting buckets into []types.StringTermsBucket")
	ErrZeroBuckets             = errors.New("zero buckets in query response")
	ErrBucketKeyNotString      = errors.New("bucket key not a string")
	ErrCastFilterAggregate     = errors.New("failed casting aggregate to *types.FilterAggregate")
	ErrCastTopMetricsAggregate = errors.New("failed casting sub aggregate to *types.TopMetricsAggregate")
	ErrCastTopHitAggregate     = errors.New("failed casting sub aggregate to *types.TopHitsAggregate")
	ErrUnmarshallTopHitValue   = errors.New("unable to unmarshall top hit value")
	ErrEmptyTopHitValueList    = errors.New("empty top hits value list")
	ErrMetricValueNotString    = errors.New("top metric value not a string")
	ErrInvalidCACert           = errors.New("no valid PEM certificates found in ca certificate file")
	ErrIncompleteCredentials   = errors.New("both username and password are required for basic authentication")
)
