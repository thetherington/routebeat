package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/thetherington/routebeat/beater/httpclient"
)

const (
	PROTO = "https"
	API   = "api/-/notify"
)

type NotifyApp struct {
	urls   []string
	Origin string
	client *http.Client
}

type NotifyAppCfg struct {
	Hosts  []string
	Origin string
}

func NewNotifierApp(cfg *NotifyAppCfg) (*NotifyApp, error) {
	app := &NotifyApp{
		Origin: cfg.Origin,
		urls:   []string{},
	}

	for _, host := range cfg.Hosts {
		app.urls = append(app.urls, fmt.Sprintf("%s://%s/%s", PROTO, host, API))
	}

	c, err := httpclient.NewHTTPClient()
	if err != nil {
		return nil, err
	}

	app.client = c

	return app, nil
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
