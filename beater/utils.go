package beater

import (
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

func updateBusNameCache() error {
	bm, err := db.QuerySchedulerEventParams()
	if err != nil || len(bm) == 0 {
		return fmt.Errorf("failed QueryScheduler(): %v", err)
	}

	cache.Load(bm)

	return nil
}
