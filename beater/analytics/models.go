package analytics

import "time"

// MultiLogResults maps each query's Key() to the slice of Source hits returned for it.
type MultiLogResults map[string][]Source

type Source struct {
	Id         string     `json:"-"`
	Timestamp  time.Time  `json:"@timestamp"`
	Annotation Annotation `json:"annotation"`
	Device     Device     `json:"device"`
	Log        Log        `json:"log"`
}

type Annotation struct {
	General General `json:"general"`
}

type General struct {
	DeviceName string `json:"device_name"`
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
