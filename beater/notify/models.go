package notify

type Notification struct {
	Message        Message        `json:"message"`
	Unique         Unique         `json:"unique"`
	Classification Classification `json:"classification"`
	Origin         Origin         `json:"origin"`
	Context        Context        `json:"context"`
	Version        string         `json:"version"`
}

type Message struct {
	Body    string `json:"body"`
	Summary string `json:"summary"`
}

type Unique struct {
	UID    string `json:"uid"`
	Expiry int    `json:"expiry"`
}

type Classification struct {
	Severity      string `json:"severity"`
	Priority      int    `json:"priority"`
	Category      string `json:"category"`
	CategoryLabel string `json:"category_label"`
}

type Detector struct {
	Host string `json:"host"`
	App  string `json:"app"`
}

type Origin struct {
	Detector Detector `json:"detector"`
}

type Details struct {
	Status    string `json:"status"`
	End       string `json:"end"`
	Trigger   string `json:"trigger"`
	EventType string `json:"eventType"`
	Source    string `json:"source"`
	Busname   string `json:"busname"`
	Start     string `json:"start"`
}

type Basic struct {
	Type    string  `json:"type"`
	Details Details `json:"details"`
}

type Detailed struct {
	Type string `json:"type"`
}

type Context struct {
	Basic    []Basic    `json:"basic"`
	Detailed []Detailed `json:"detailed"`
}
