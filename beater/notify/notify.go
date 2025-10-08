package notify

import "fmt"

const (
	PROTO = "https"
	API   = "api/-/notify"
)

type NotifyApp struct {
	urls   []string
	Origin string
}

type NotifyAppCfg struct {
	Hosts  []string
	Origin string
}

func NewNotifierApp(cfg *NotifyAppCfg) *NotifyApp {
	app := &NotifyApp{
		Origin: cfg.Origin,
		urls:   []string{},
	}

	for _, host := range cfg.Hosts {
		app.urls = append(app.urls, fmt.Sprintf("%s://%s/%s", PROTO, host, API))
	}

	return app
}
