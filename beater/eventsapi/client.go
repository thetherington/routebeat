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
	"github.com/thetherington/routebeat/beater/schedule"
)

const (
	defaultTimeout     = 3 * time.Second
	defaultBulkTimeout = 15 * time.Second

	defaultStartParam  = "start"
	defaultEndParam    = "end"
	defaultOutputParam = "output"
	defaultTaskParam   = "task"
)

// Config for the events api client
type Config struct {
	Url         string        // base url. query parameters are added to any already in the url
	Timeout     time.Duration // per-output refresh
	BulkTimeout time.Duration // all events at a point in time (a much larger response)
	Headers     map[string]string

	// query parameter names, and the task value used to filter the events. an empty Task
	// sends no task filter
	StartParam  string
	EndParam    string
	OutputParam string
	TaskParam   string
	Task        string
}

// Client fetches scheduler events from the events api
type Client struct {
	client      *http.Client
	base        *url.URL
	headers     map[string]string
	timeout     time.Duration
	bulkTimeout time.Duration

	startParam  string
	endParam    string
	outputParam string
	taskParam   string
	task        string
}

// New creates an events api client
func New(cfg Config) (*Client, error) {
	if cfg.Url == "" {
		return nil, errors.New("events api url is required")
	}

	if strings.Contains(cfg.Url, "{output}") {
		return nil, errors.New("events api url can't use the {output} placeholder, the output is sent as a query parameter")
	}

	base, err := url.Parse(cfg.Url)
	if err != nil {
		return nil, fmt.Errorf("invalid events api url: %w", err)
	}

	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid events api url %q: scheme and host are required", cfg.Url)
	}

	client, err := httpclient.NewHTTPClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create events api http client: %w", err)
	}

	return &Client{
		client:      client,
		base:        base,
		headers:     cfg.Headers,
		timeout:     withDefault(cfg.Timeout, defaultTimeout),
		bulkTimeout: withDefault(cfg.BulkTimeout, defaultBulkTimeout),
		startParam:  withDefault(cfg.StartParam, defaultStartParam),
		endParam:    withDefault(cfg.EndParam, defaultEndParam),
		outputParam: withDefault(cfg.OutputParam, defaultOutputParam),
		taskParam:   withDefault(cfg.TaskParam, defaultTaskParam),
		task:        cfg.Task,
	}, nil
}

func withDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

// EventsAt fetches the scheduler events for every output that are active at a point in time.
// at is passed through to the api as-is, e.g. "now" or "now+25m". uses the bulk timeout
func (c *Client) EventsAt(ctx context.Context, at string) ([]schedule.Event, schedule.Stats, error) {
	return c.events(ctx, url.Values{c.startParam: {at}, c.endParam: {at}}, c.bulkTimeout)
}

// OutputEventsAt fetches the scheduler events for one output that are active at a point in time,
// the same request as EventsAt scoped to the output. a start and end are always sent: without them
// the api returns every event it holds for the output (the previous day and the next 6 days)
func (c *Client) OutputEventsAt(ctx context.Context, output, at string) ([]schedule.Event, schedule.Stats, error) {
	return c.events(ctx, url.Values{c.outputParam: {output}, c.startParam: {at}, c.endParam: {at}}, c.timeout)
}

// RequestURL builds the request url: the base url with the params and the task filter added
func (c *Client) RequestURL(params url.Values) string {
	u := *c.base

	q := u.Query()
	for k, v := range params {
		q[k] = v
	}

	if c.task != "" {
		q.Set(c.taskParam, c.task)
	}

	u.RawQuery = q.Encode()

	return u.String()
}

// events requests the url built from the params and parses the response into apg events
func (c *Client) events(ctx context.Context, params url.Values, timeout time.Duration) ([]schedule.Event, schedule.Stats, error) {
	body, err := c.fetch(ctx, c.RequestURL(params), timeout)
	if err != nil {
		return nil, schedule.Stats{}, err
	}

	events, stats, err := schedule.ParseEvents(body)
	if err != nil {
		return nil, stats, fmt.Errorf("failed to parse events api response: %w", err)
	}

	return events, stats, nil
}

// fetch sends the GET request and returns the response body of a 2xx response
func (c *Client) fetch(ctx context.Context, reqURL string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create events api request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("events api request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("events api request failed: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read events api response: %w", err)
	}

	return body, nil
}
