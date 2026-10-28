package analytics

import "errors"

var (
	ErrNoResults             = errors.New("no results found")
	ErrSearchFailed          = errors.New("search query failed")
	ErrInvalidConfig         = errors.New("invalid configuration")
	ErrInvalidCACert         = errors.New("no valid PEM certificates found in ca certificate file")
	ErrIncompleteCredentials = errors.New("both username and password are required for basic authentication")
)
