package httpclient

import (
	"context"
	"crypto/tls"
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

func init() {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
}

func NewHTTPClient(credentials ...*MagnumAuthCredentials) (*http.Client, error) {
	var err error

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	c := &http.Client{Jar: jar}

	if len(credentials) > 0 {
		err = magnumAuth(credentials[0], jar)
	}

	return c, err
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
