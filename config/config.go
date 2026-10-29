// Config is put into a different package to prevent cyclic imports in case
// it is needed in several locations

package config

import "time"

type MagnumOIDCAuth struct {
	ClientID     string `config:"client_id"`
	ClientSecret string `config:"client_secret"`
	TokenURL     string `config:"token_url"`
}

type MagnumAPI struct {
	Url              string         `config:"url"`
	Limit            int            `config:"limit"`
	Notifications    bool           `config:"notifications"`
	SerializeQueries bool           `config:"serialize_queries"`
	Auth             MagnumOIDCAuth `config:"auth"`
	Timeout          time.Duration  `config:"timeout"`
	SubReconnectIdle time.Duration  `config:"sub_reconnect_idle"`
}

type Mapping struct {
	Nameset string `config:"nameset"`
	Default string `config:"default"`
}

type Developer struct {
	LoadCache       bool `config:"load_cache"`
	SaveCache       bool `config:"save_cache"`
	ValidateEndDate bool `config:"validate_end_date"`
}

type Elasticsearch struct {
	Address  string        `config:"address"`
	Index    string        `config:"index"`
	Username string        `config:"username"`
	Password string        `config:"password"`
	APIKey   string        `config:"api_key"`
	SSL      ESSSL         `config:"ssl"`
	Period   time.Duration `config:"period"`
	Dev      Developer     `config:"dev"`
}

type ESSSL struct {
	CACert             string `config:"ca_cert"`
	InsecureSkipVerify bool   `config:"insecure_skip_verify"`
}

type Notifier struct {
	Manual []Node       `config:"manual"`
	Auto   NotifierAuto `config:"auto"`
}

type Node struct {
	Node string `config:"node"`
	Port int    `config:"port"`
}

type NotifierAuto struct {
	Host          string   `config:"host"`
	Username      string   `config:"username"`
	Password      string   `config:"password"`
	NotifierTypes []string `config:"types"`
}

type Webhook struct {
	Enabled bool          `config:"enabled"`
	Port    int           `config:"port"`
	Wait    time.Duration `config:"wait"` // time an unscheduled state waits for webhook data
}

type EventsAPI struct {
	Url     string            `config:"url"` // empty url disables the events api
	Timeout time.Duration     `config:"timeout"`
	Headers map[string]string `config:"headers"`
}

type Config struct {
	Period    time.Duration `config:"period"`
	Tags      []string      `config:"tags"`
	Mapping   *Mapping      `config:"mapping"`
	API       MagnumAPI     `config:"api"`
	ES        Elasticsearch `config:"elasticsearch"`
	Notifiers Notifier      `config:"notifiers"`
	TDA       string        `config:"tda"`
	Zorro     string        `config:"zorro"`
	Webhook   Webhook       `config:"webhook"`
	EventsAPI EventsAPI     `config:"events_api"`
}

var DefaultConfig = Config{
	Period: 10 * time.Second,
	Tags:   []string{},
	API: MagnumAPI{
		Url:              "https://129.153.131.121/graphql/v1.1",
		Limit:            2000,
		Notifications:    true,
		SerializeQueries: false,
		Auth: MagnumOIDCAuth{
			ClientID:     "insite-poller",
			ClientSecret: "QdS1US0v2xABh4d5CliQAWZrmSGPMOxd",
			TokenURL:     "https://129.153.131.121/auth/realms/magnum/protocol/openid-connect/token",
		},
		Timeout:          10 * time.Second,
		SubReconnectIdle: 120 * time.Minute,
	},
	ES: Elasticsearch{
		Address: "http://127.0.0.1:9200",
		Index:   "log-magnum-scheduler-*",
		Period:  5 * time.Minute,
		Dev: Developer{
			LoadCache:       false,
			SaveCache:       false,
			ValidateEndDate: true,
		},
	},
	Notifiers: Notifier{
		Manual: []Node{},
		Auto: NotifierAuto{
			Username:      "admin",
			Password:      "admin",
			NotifierTypes: []string{"notification-elastic"},
		},
	},
	Webhook: Webhook{
		Enabled: true,
		Port:    8082,
		Wait:    5 * time.Second,
	},
	EventsAPI: EventsAPI{
		Timeout: 3 * time.Second,
	},
}
