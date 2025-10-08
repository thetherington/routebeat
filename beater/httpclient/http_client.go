package httpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"golang.org/x/oauth2/clientcredentials"
)

type MagnumAuthCredentials struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	Done         chan struct{}
}

type AnalyticsAuthCredentials struct {
	Username string
	Password string
	IP       string
}

type HTTPClientOption func(*http.Client) error

func init() {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
}

func NewHTTPClient(opts ...HTTPClientOption) (*http.Client, error) {
	var err error

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	c := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return c, err
}

func WithMagnumAuth(creds *MagnumAuthCredentials) HTTPClientOption {
	return func(c *http.Client) error {
		jar, ok := c.Jar.(*cookiejar.Jar)
		if !ok {
			return fmt.Errorf("http client jar is not a cookiejar.Jar")
		}
		return magnumAuth(creds, jar)
	}
}

func WithAnalyticsAuth(creds *AnalyticsAuthCredentials) HTTPClientOption {
	return func(c *http.Client) error {
		return AuthenticateAnalytics(c, creds)
	}
}

// AuthenticateAnalytics performs authentication and stores the session cookie in the client.
func AuthenticateAnalytics(c *http.Client, creds *AnalyticsAuthCredentials) error {
	loginURL := fmt.Sprintf("https://%s:443/api/v1/login", creds.IP)

	body := map[string]string{
		"username": creds.Username,
		"password": creds.Password,
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal analytics login body: %w", err)
	}

	req, err := http.NewRequest("POST", loginURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create analytics login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("analytics login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("analytics login failed: %s", resp.Status)
	}

	// Cookies are automatically stored in the jar for the host
	return nil
}

func magnumAuth(c *MagnumAuthCredentials, jar *cookiejar.Jar) error {
	token, err := getNewToken(c.ClientID, c.ClientSecret, c.TokenURL)
	if err != nil {
		return err
	}

	u, err := url.Parse(c.TokenURL)
	if err != nil {
		return err
	}

	cookieURL, _ := url.Parse(fmt.Sprintf("%s://%s", u.Scheme, u.Host))
	jar.SetCookies(cookieURL, []*http.Cookie{{Name: "magoidc-token", Value: token}})

	go func() {
		ticker := time.NewTicker(4 * time.Minute)

		for {
			select {
			// done channel from the main beater
			case <-c.Done:
				logp.Warn("exiting token auth routine")
				return

			case <-ticker.C:
				token, err := getNewToken(c.ClientID, c.ClientSecret, c.TokenURL)
				if err != nil {
					logp.Err("create auth token failure: %v", err)
					continue
				}

				jar.SetCookies(cookieURL, []*http.Cookie{{Name: "magoidc-token", Value: token}})
			}
		}
	}()

	return nil
}

func getNewToken(id, secret, url string) (string, error) {
	config := clientcredentials.Config{
		ClientID:     id,
		ClientSecret: secret,
		TokenURL:     url,
	}

	tokenSource := config.TokenSource(context.Background())

	token, err := tokenSource.Token()
	if err != nil {
		return "", err
	}

	return token.AccessToken, nil
}
