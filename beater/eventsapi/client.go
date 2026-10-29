package eventsapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thetherington/routebeat/beater/httpclient"
)

// CallbackFunc is executed with the raw response payload bytes after a successful request.
// It should contain parsing and business processing logic.
type CallbackFunc func(ctx context.Context, payload []byte) error

// placeholder in the url that is replaced with the destination (output) name
const OutputPlaceholder = "{output}"

const defaultTimeout = 3 * time.Second

// Config for the events api client
type Config struct {
	Url     string
	Timeout time.Duration
	Headers map[string]string
}

// Client fetches scheduler events on demand for a destination and passes the
// response payload into the callback function
type Client struct {
	client   *http.Client
	url      string
	headers  map[string]string
	timeout  time.Duration
	callback CallbackFunc
}

// New creates an events api client that runs the callback with each response payload
func New(cfg Config, callback CallbackFunc) (*Client, error) {
	if cfg.Url == "" {
		return nil, errors.New("events api url is required")
	}

	if callback == nil {
		return nil, errors.New("events api callback is required")
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}

	client, err := httpclient.NewHTTPClient(httpclient.WithTimeout(cfg.Timeout))
	if err != nil {
		return nil, fmt.Errorf("failed to create events api http client: %w", err)
	}

	return &Client{
		client:   client,
		url:      cfg.Url,
		headers:  cfg.Headers,
		timeout:  cfg.Timeout,
		callback: callback,
	}, nil
}

// requestURL builds the request url for the output. the output replaces the {output} placeholder
// if present, otherwise it is added as the "output" query parameter
func (c *Client) requestURL(output string) (string, error) {
	if strings.Contains(c.url, OutputPlaceholder) {
		return strings.ReplaceAll(c.url, OutputPlaceholder, url.PathEscape(output)), nil
	}

	u, err := url.Parse(c.url)
	if err != nil {
		return "", err
	}

	q := u.Query()
	q.Set("output", output)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// Refresh fetches the scheduler events for the output and runs the callback with the response payload
func (c *Client) Refresh(ctx context.Context, output string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	reqURL, err := c.requestURL(output)
	if err != nil {
		return fmt.Errorf("failed to build events api url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create events api request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("events api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("events api request failed: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read events api response: %w", err)
	}

	return c.callback(ctx, body)
}
