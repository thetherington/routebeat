package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/thetherington/routebeat/beater/httpclient"
)

const (
	PROTO = "https"
	API   = "api/-/notify"
)

type NotiferInterface interface {
	Send(notifications ...Notification) error
	GetOrigin() string
}

type NotifyApp struct {
	urls   []string
	Origin string
	client *http.Client
}

type NotifyAppCfg struct {
	Nodes  []Node
	Origin string
}

type Node struct {
	Node string `config:"node"`
	Port int    `config:"port"`
}

type AutoDiscoveryCfg struct {
	Host          string
	Username      string
	Password      string
	NotifierTypes []string
	Origin        string
}

type NotifierAppOption func(n *NotifyApp) error

type locatorPayload struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

func NewNotifierApp(opts NotifierAppOption) (*NotifyApp, error) {
	app := &NotifyApp{}

	if err := opts(app); err != nil {
		return nil, err
	}

	return app, nil
}

func WithManualConfig(cfg *NotifyAppCfg) NotifierAppOption {
	return func(n *NotifyApp) error {
		n.Origin = cfg.Origin
		n.urls = []string{}

		for _, node := range cfg.Nodes {
			n.urls = append(n.urls, fmt.Sprintf("%s://%s:%d/%s", PROTO, node.Node, node.Port, API))
		}

		c, err := httpclient.NewHTTPClient()
		if err != nil {
			return err
		}

		n.client = c

		return nil
	}
}

func WithAutoDiscover(cfg *AutoDiscoveryCfg) NotifierAppOption {
	return func(n *NotifyApp) error {
		n.Origin = cfg.Origin
		n.urls = []string{}

		client, err := httpclient.NewHTTPClient(httpclient.WithAnalyticsAuth(
			&httpclient.AnalyticsAuthCredentials{
				Username: cfg.Username,
				Password: cfg.Password,
				IP:       cfg.Host,
			},
		))
		if err != nil {
			return fmt.Errorf("failed to create http client for analytics auth: %w", err)
		}

		for _, notifierType := range cfg.NotifierTypes {
			result, err := useLocator(&LocatorRequest{
				ip:     cfg.Host,
				lookup: notifierType,
				client: client,
			})
			if err != nil {
				return err
			}

			n.urls = append(n.urls, fmt.Sprintf("%s://%s:%d/%s", PROTO, result.IP, result.Port, API))
		}

		n.client = client

		return nil
	}
}

type LocatorRequest struct {
	ip     string
	lookup string
	client *http.Client
}

func useLocator(req *LocatorRequest) (*locatorPayload, error) {
	base := &url.URL{
		Scheme: "https",
		Host:   req.ip,
		Path:   "/api/-/model/nature/locator/by-type",
	}

	q := url.Values{}
	q.Add("type", req.lookup)

	base.RawQuery = q.Encode()

	http_request, err := http.NewRequest(http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create analytics locate request request: %w", err)
	}
	http_request.Header.Set("Content-Type", "application/json")

	resp, err := req.client.Do(http_request)
	if err != nil {
		return nil, fmt.Errorf("failed to locate notifier program for: %s, %w", req.lookup, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("non-2xx response locating notifier program for %s: %s", req.lookup, resp.Status)
	}

	var v *locatorPayload

	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("failed to decode analytics locate response for %s: %w", req.lookup, err)
	}

	return v, nil
}

type NotifyError struct {
	Errors []error
}

func (e *NotifyError) Error() string {
	msg := "notify send errors:"
	for _, err := range e.Errors {
		msg += "\n - " + err.Error()
	}
	return msg
}

func (n *NotifyApp) Post(notification Notification) []error {
	var wg sync.WaitGroup

	errs := make([]error, len(n.urls))

	body, err := json.Marshal(notification)
	if err != nil {
		for i := range errs {
			errs[i] = fmt.Errorf("failed to marshal notification: %w", err)
		}
		return errs
	}

	for i, url := range n.urls {
		wg.Add(1)

		go func(idx int, u string) {
			defer wg.Done()
			req, err := http.NewRequest("POST", u, bytes.NewBuffer(body))
			if err != nil {
				errs[idx] = fmt.Errorf("failed to create request for %s: %w", u, err)
				return
			}

			req.Header.Set("Content-Type", "application/json")

			resp, err := n.client.Do(req)
			if err != nil {
				errs[idx] = fmt.Errorf("http post failed for %s: %w", u, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				errs[idx] = fmt.Errorf("non-2xx response from %s: %s", u, resp.Status)
			}
		}(i, url)
	}
	wg.Wait()
	return errs
}

func (n *NotifyApp) Send(notifications ...Notification) error {
	var allErrs []error

	for _, notification := range notifications {
		errs := n.Post(notification)
		for _, e := range errs {
			if e != nil {
				allErrs = append(allErrs, e)
			}
		}
	}

	if len(allErrs) > 0 {
		return &NotifyError{Errors: allErrs}
	}

	return nil
}

func (n *NotifyApp) GetOrigin() string {
	return n.Origin
}
