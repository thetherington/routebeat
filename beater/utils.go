package beater

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

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

// ProcessResults debug selectors. each event type logs under its own sub selector
const (
	processResultsSelectorBase         = "ProcessResults"
	processResultsSelectorWildcard     = "ProcessResults.*"
	processResultsSelectorQuery        = "ProcessResults.Query"
	processResultsSelectorNotification = "ProcessResults.Notifications"
)

// processResultsSelector returns the debug selector to log the event type's results under.
// libbeat matches selectors exactly (only "*" is a wildcard), so the logs go under whichever of the
// sub selector, "ProcessResults.*" or "ProcessResults" is enabled, letting either enable every sub selector
func processResultsSelector(eventType EventType) string {
	sub := processResultsSelectorQuery
	if eventType == Notification {
		sub = processResultsSelectorNotification
	}

	for _, s := range []string{sub, processResultsSelectorWildcard, processResultsSelectorBase} {
		if logp.HasSelector(s) {
			return s
		}
	}

	return sub
}

// eventDestination returns the event's destination label, or the magnum destination name when mapping is disabled
func eventDestination(event *beat.Event) string {
	if v, err := event.Fields.GetValue("destinationLabel"); err == nil {
		if label, ok := v.(string); ok && label != "" {
			return label
		}
	}

	name, _ := event.Fields.GetValue("dstName")
	s, _ := name.(string)

	return s
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

// Single-item JSON-based converter
func ConvertByJSON[S any, D any](src S) (D, error) {
	var dst D

	b, err := json.Marshal(src)

	if err != nil {
		return dst, err
	}

	if err := json.Unmarshal(b, &dst); err != nil {
		return dst, err
	}

	return dst, nil
}

// Slice JSON-based converter (marshals once)
func ConvertSliceJSON[S any, D any](src []S) ([]D, error) {
	if src == nil {
		return nil, nil
	}

	var dst []D

	b, err := json.Marshal(src)

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(b, &dst); err != nil {
		return nil, err
	}

	return dst, nil
}
