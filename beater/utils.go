package beater

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

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
