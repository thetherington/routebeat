package analytics

import "time"

type Source struct {
	Id     string `json:"-"`
	Device Device `json:"device"`
	Log    Log    `json:"log"`
}

type Device struct {
	Timestamp time.Time `json:"timestamp"`
}

type Syslog struct {
	Message string `json:"message"`
}

type Log struct {
	Syslog Syslog `json:"syslog"`
}
