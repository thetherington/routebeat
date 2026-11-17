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
	LoadCache       bool `config:"load_cache"`        // use the saved schedule store and skip the events api poll
	ValidateEndDate bool `config:"validate_end_date"` // only use schedule windows that are active now
}

// Schedule controls the events api poll that builds the schedule store
type Schedule struct {
	Period    time.Duration `config:"period"`    // how often all events are fetched
	Lookahead time.Duration `config:"lookahead"` // second fetch at now+lookahead for the upcoming windows
	Dev       Developer     `config:"dev"`
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

type EventsAPI struct {
	Url         string            `config:"url"`          // required. the schedule source
	Timeout     time.Duration     `config:"timeout"`      // each of the two per-output recheck requests when a bus goes unscheduled
	BulkTimeout time.Duration     `config:"bulk_timeout"` // schedule poll of all events
	Delay       time.Duration     `config:"delay"`        // pause before a refresh, to allow the api to receive new data
	Headers     map[string]string `config:"headers"`

	// query parameter names, and the task value used to filter the events ("" sends no task filter)
	StartParam  string `config:"start_param"`
	EndParam    string `config:"end_param"`
	OutputParam string `config:"output_param"`
	TaskParam   string `config:"task_param"`
	Task        string `config:"task"`
}

type Config struct {
	Period    time.Duration `config:"period"`
	Tags      []string      `config:"tags"`
	Mapping   *Mapping      `config:"mapping"`
	API       MagnumAPI     `config:"api"`
	Schedule  Schedule      `config:"schedule"`
	Notifiers Notifier      `config:"notifiers"`
	TDA       string        `config:"tda"`
	Zorro     string        `config:"zorro"`
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
	Schedule: Schedule{
		Period:    5 * time.Minute,
		Lookahead: 25 * time.Minute,
		Dev: Developer{
			LoadCache:       false,
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
	EventsAPI: EventsAPI{
		Timeout:     3 * time.Second,
		BulkTimeout: 15 * time.Second,
		StartParam:  "start",
		EndParam:    "end",
		OutputParam: "output",
		TaskParam:   "task",
		Task:        "apg",
	},
}
