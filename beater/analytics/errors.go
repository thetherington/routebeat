package analytics

import "errors"

var (
	ErrNoHitsReturned        = errors.New("no hits returned in query")
	ErrCastAggResponse       = errors.New("failed casting response into *types.StringTermsAggregate")
	ErrCastStringTermsBucket = errors.New("failed casting buckets into []types.StringTermsBucket")
	ErrZeroBuckets           = errors.New("zero buckets in query response")
	ErrBucketKeyNotString    = errors.New("bucket key not a string")
	ErrCastFilterAggregate   = errors.New("failed casting aggregate to *types.FilterAggregate")
	ErrCastTopHitAggregate   = errors.New("failed casting sub aggregate to *types.TopHitsAggregate")
	ErrUnmarshallTopHitValue = errors.New("unable to unmarshall top hit value")
	ErrEmptyTopHitValueList  = errors.New("empty top hits value list")
)
