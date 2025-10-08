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
	Url           string         `config:"url"`
	Limit         int            `config:"limit"`
	Notifications bool           `config:"notifications"`
	Auth          MagnumOIDCAuth `config:"auth"`
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
	Address string        `config:"address"`
	Index   string        `config:"index"`
	Period  time.Duration `config:"period"`
	Dev     Developer     `config:"dev"`
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

type Config struct {
	Period    time.Duration `config:"period"`
	Tags      []string      `config:"tags"`
	Mapping   *Mapping      `config:"mapping"`
	API       MagnumAPI     `config:"api"`
	ES        Elasticsearch `config:"elasticsearch"`
	Notifiers Notifier      `config:"notifiers"`
	TDA       string        `config:"tda"`
	Zorro     string        `config:"zorro"`
}

var DefaultConfig = Config{
	Period: 10 * time.Second,
	Tags:   []string{},
	API: MagnumAPI{
		Url:           "https://129.153.131.121/graphql/v1.1",
		Limit:         2000,
		Notifications: true,
		Auth: MagnumOIDCAuth{
			ClientID:     "insite-poller",
			ClientSecret: "QdS1US0v2xABh4d5CliQAWZrmSGPMOxd",
			TokenURL:     "https://129.153.131.121/auth/realms/magnum/protocol/openid-connect/token",
		},
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
}
