package analytics

import "errors"

var (
	ErrNoResults     = errors.New("no results found")
	ErrSearchFailed  = errors.New("search query failed")
	ErrInvalidConfig = errors.New("invalid configuration")
)
