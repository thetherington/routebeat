package beater

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

// converts a url to websocket scheme url
func getWssURL(s string) string {
	// replace the "https" in the api url with "wss"
	u, _ := url.Parse(s)
	u.Scheme = "wss"

	return u.String()
}

func rangeOverNamesets(namesetName []NamesetName, m *mapstr.M) {
	for _, n := range namesetName {
		m.Put(
			fmt.Sprintf("nameset.%s", strings.ToLower(n.Nameset.Name)),
			n.Name,
		)
	}
}

func findNamesetValueByName(s string, namesetName []NamesetName, defaultValue string) string {
	for _, n := range namesetName {
		if n.Nameset.Name == s {
			return n.Name
		}
	}

	return defaultValue
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// getSrcDstIds extracts srcId and dstId from a beat.Event, handling both possible srcId locations.
func getSrcDstIds(event *beat.Event) (srcId, dstId string, err error) {
	dst, err := event.GetValue("dstId")
	if err != nil {
		return "", "", fmt.Errorf("failed to get dstId: %w", err)
	}

	src, err := event.GetValue("routeableTerminal.subscribedSource.srcId")
	if err != nil {
		src, err = event.GetValue("routeableTerminal.physicalSource.srcId")
		if err != nil {
			return "", "", fmt.Errorf("failed to get srcId: %w", err)
		}
	}

	srcStr, ok1 := src.(string)
	dstStr, ok2 := dst.(string)
	if !ok1 || !ok2 {
		return "", "", fmt.Errorf("srcId or dstId is not a string")
	}

	return srcStr, dstStr, nil
}

// HandleAnalyzeLogError logs the appropriate message for AnalyzeLogCollection errors.
func HandleAnalyzeLogError(err error, srcId, dstId string, event *beat.Event) {
	if errors.Is(err, ErrNoLogsFound) {
		logp.Debug("AnalyzeLogCollection", "No logs found for event with srcId: %s and dstId: %s", srcId, dstId)
	} else if errors.Is(err, ErrNoRequestLogs) {
		logp.Debug("AnalyzeLogCollection", "No request logs found for event with srcId: %s and dstId: %s", srcId, dstId)
	} else if errors.Is(err, ErrNoCompletedLogs) {
		logp.Debug("AnalyzeLogCollection", "No completed logs found for event with srcId: %s and dstId: %s", srcId, dstId)
	} else {
		logp.Err("Failed to analyze log collection for event: %v %s", event, err.Error())
	}
}
