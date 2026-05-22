package beater

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

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
		return "", "", fmt.Errorf("event.GetValue: failed to get dstId: %w", err)
	}

	src, err := event.GetValue("routeableTerminal.subscribedSource.srcId")
	if err != nil {
		src, err = event.GetValue("routeableTerminal.physicalSource.srcId")
		if err != nil {
			return "", "", fmt.Errorf("event.GetValue: failed to get srcId: %w", err)
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
func HandleAnalyzeLogError(err error, event *beat.Event) {
	srcId, dstId, _ := getSrcDstIds(event)

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

// Extracts output from magnum port terminal by the last number from a string like "[111,8,2,32]".
func ExtractOutputFromPort(s string) (int, error) {
	trimmed := strings.TrimFunc(s, func(r rune) bool {
		return !unicode.IsDigit(r) && r != ',' && r != '-'
	})
	parts := strings.Split(trimmed, ",")
	if len(parts) == 0 {
		return 0, fmt.Errorf("no numbers found")
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	return strconv.Atoi(last)
}

// ParseMulticastAddress extracts the multicast IP address from a string like "DST IP: 239.32.103.143:5004".
func ParseMulticastAddress(s string) (string, error) {
	prefix := "DST IP: "
	if !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("string does not start with expected prefix: %s", prefix)
	}
	addrPort := strings.TrimPrefix(s, prefix)
	parts := strings.Split(addrPort, ":")
	if len(parts) < 1 {
		return "", fmt.Errorf("invalid address format")
	}
	return parts[0], nil
}

// ExtractStringFromEvent extracts a string value from the event for the given field path.
func ExtractStringFromEvent(event *beat.Event, fieldPath string) (string, error) {
	value, err := event.GetValue(fieldPath)
	if err != nil {
		return "", fmt.Errorf("event.GetValue: error: %s", err.Error())
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", fieldPath)
	}
	return str, nil
}
