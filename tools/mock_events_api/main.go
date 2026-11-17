// mock_events_api is a local stand-in for the scheduler events api used by the routebeat events_api client.
// it answers GETs with a MAIN and BACKUP apg event per output, active around the requested time:
//
//   - with ?output=<bus> (unscheduled recheck), events for that output active at ?start=
//   - without output (schedule poll), events for every -outputs bus active at ?start=
//
// ?start= is "now" or "now+<duration>" ("now+25m"), and defaults to now.
//
// a ?task= other than apg returns no events, the way the real api filters by task.
//
//	go run ./tools/mock_events_api -addr :8090 -primary DCXA006D -outputs BSITAL,BSTRJA
//
//	curl 'http://localhost:8090/events?output=BSITAL&start=now&end=now&task=apg'
//	curl 'http://localhost:8090/events?start=now%2B25m&end=now%2B25m&task=apg'
//
// routebeat.yml:
//
//	events_api:
//	  url: "http://localhost:8090/events"
package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const schedulerDateLayout = "2006/01/02 15:04:05"

type eventParams struct {
	ApgID    string `json:"apg_id"`
	ApgRule  string `json:"apg_rule"`
	BusName  string `json:"bus_name"`
	Historic bool   `json:"historic"`
	PriSrc   string `json:"pri_src"`
	SecSrc   string `json:"sec_src"`
}

type event struct {
	Comment        string      `json:"comment"`
	End            int64       `json:"end"`
	EndDate        string      `json:"end_date"`
	EndOffset      string      `json:"end_offset"`
	Entry          string      `json:"entry"`
	Error          string      `json:"error"`
	EventID        string      `json:"event_id"`
	EventParams    eventParams `json:"event_params"`
	History        []any       `json:"history"`
	Input          string      `json:"input"`
	InputBlocking  bool        `json:"input_blocking"`
	InputID        string      `json:"input_id"`
	InputPooled    bool        `json:"input_pooled"`
	InputType      string      `json:"input_type"`
	Output         string      `json:"output"`
	OutputBlocking bool        `json:"output_blocking"`
	OutputID       string      `json:"output_id"`
	OutputPooled   bool        `json:"output_pooled"`
	OutputType     string      `json:"output_type"`
	Recur          struct{}    `json:"recur"`
	ScheduleID     string      `json:"schedule_id"`
	Start          int64       `json:"start"`
	StartDate      string      `json:"start_date"`
	StartOffset    string      `json:"start_offset"`
	State          int         `json:"state"`
	Tags           []string    `json:"tags"`
	Task           string      `json:"task"`
}

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	primary := flag.String("primary", "DCXA006D", "primary (MAIN) source label")
	backup := flag.String("backup", "", "backup (BACKUP) source label, random per request when empty")
	before := flag.Duration("before", 30*time.Minute, "event window starts this long before now")
	after := flag.Duration("after", 30*time.Minute, "event window ends this long after now")
	outputs := flag.String("outputs", "BSITAL,BSTRJA", "comma separated buses returned by a request without an output")
	noise := flag.Bool("noise", false, "also return a non-apg event when no task filter is sent, to exercise the client filtering")
	flag.Parse()

	handler := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		at, err := relativeTime(q.Get("start"))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}

		buses := strings.Split(*outputs, ",")
		if output := q.Get("output"); output != "" {
			buses = []string{output}
		}

		sec := *backup
		if sec == "" {
			sec = randomSource()
		}

		start, end := at.Add(-*before), at.Add(*after)

		payload := []any{}

		task := q.Get("task")
		if task == "" || task == "apg" {
			for _, bus := range buses {
				payload = append(payload,
					newEvent(bus, *primary, sec, "MAIN", start, end),
					newEvent(bus, sec, *primary, "BACKUP", start, end),
				)
			}
		}

		if *noise && task == "" {
			payload = append(payload, map[string]any{"task": "route", "output": buses[0], "input": randomSource()})
		}

		log.Printf("%s %s -> outputs=%s primary=%s backup=%s window=%s..%s events=%d",
			r.Method, r.URL, strings.Join(buses, ","), *primary, sec,
			start.Format(schedulerDateLayout), end.Format(schedulerDateLayout), len(payload))

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			log.Printf("write failed: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", handler)

	log.Printf("mock events api listening on %s (primary %s)", *addr, *primary)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// relativeTime resolves "", "now" or "now+<duration>" (e.g. now+25m) to a time
func relativeTime(s string) (time.Time, error) {
	now := time.Now().UTC().Truncate(time.Second)

	switch {
	case s == "" || s == "now":
		return now, nil
	case strings.HasPrefix(s, "now+"):
		d, err := time.ParseDuration(strings.TrimPrefix(s, "now+"))
		if err != nil {
			return now, fmt.Errorf("invalid relative time %q", s)
		}
		return now.Add(d), nil
	}

	return now, fmt.Errorf("invalid relative time %q", s)
}

// newEvent builds an apg event shaped like the real scheduler payload (start/end as epoch-ns numbers)
func newEvent(output, input, other, tag string, start, end time.Time) event {
	startNs := start.UnixNano()

	return event{
		End:       end.UnixNano(),
		EndDate:   end.Format(schedulerDateLayout),
		EndOffset: "0",
		Entry:     "manual",
		EventID:   uuid(),
		EventParams: eventParams{
			ApgID:    randomDigits(8),
			ApgRule:  "Pad1Pad2",
			BusName:  "{event.output}",
			Historic: true,
			PriSrc:   "{event.input}",
			SecSrc:   other,
		},
		History:        []any{},
		Input:          input,
		InputID:        uuid(),
		InputType:      "magnum",
		Output:         output,
		OutputBlocking: true,
		OutputID:       uuid(),
		OutputType:     "magnum",
		ScheduleID:     randomDigits(8) + "&" + strconv.FormatInt(startNs, 10),
		Start:          startNs,
		StartDate:      start.Format(schedulerDateLayout),
		StartOffset:    "0",
		State:          1,
		Tags:           []string{tag},
		Task:           "apg",
	}
}

// randomSource returns a source label like the lab's remote sources, e.g. REM546
func randomSource() string {
	return "REM" + randomDigits(3)
}

func randomDigits(n int) string {
	s := make([]byte, n)
	for i := range s {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		s[i] = byte('0' + d.Int64())
	}
	return string(s)
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
