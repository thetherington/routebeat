// mock_events_api is a local stand-in for the scheduler events api used by the routebeat events_api client.
// it answers GETs with a MAIN and BACKUP apg event for the requested output, active around the current time.
//
//	go run ./tools/mock_events_api -addr :8090 -primary DCXA006D
//
//	curl 'http://localhost:8090/events?output=BSITAL'
//	curl 'http://localhost:8090/events/BSITAL'
//
// routebeat.yml:
//
//	events_api:
//	  url: "http://localhost:8090/events/{output}"
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
	End            string      `json:"end"`
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
	Start          string      `json:"start"`
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
	noise := flag.Bool("noise", false, "also return a non-apg event, to exercise the callback filtering")
	flag.Parse()

	handler := func(w http.ResponseWriter, r *http.Request) {
		output := r.PathValue("output")
		if output == "" {
			output = r.URL.Query().Get("output")
		}

		if output == "" {
			http.Error(w, `{"error":"output is required"}`, http.StatusBadRequest)
			return
		}

		sec := *backup
		if sec == "" {
			sec = randomSource()
		}

		now := time.Now().UTC().Truncate(time.Second)
		start, end := now.Add(-*before), now.Add(*after)

		events := []event{
			newEvent(output, *primary, sec, "MAIN", start, end),
			newEvent(output, sec, *primary, "BACKUP", start, end),
		}

		var payload []any
		for _, e := range events {
			payload = append(payload, e)
		}

		if *noise {
			payload = append(payload, map[string]any{"task": "route", "output": output, "input": randomSource()})
		}

		log.Printf("%s %s -> output=%s primary=%s backup=%s window=%s..%s",
			r.Method, r.URL, output, *primary, sec, start.Format(schedulerDateLayout), end.Format(schedulerDateLayout))

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			log.Printf("write failed: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", handler)
	mux.HandleFunc("GET /events/{output}", handler)

	log.Printf("mock events api listening on %s (primary %s)", *addr, *primary)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// newEvent builds an apg event shaped like the real scheduler payload (start/end as epoch-ns strings)
func newEvent(output, input, other, tag string, start, end time.Time) event {
	startNs := strconv.FormatInt(start.UnixNano(), 10)

	return event{
		End:       strconv.FormatInt(end.UnixNano(), 10),
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
		ScheduleID:     randomDigits(8) + "&" + startNs,
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
