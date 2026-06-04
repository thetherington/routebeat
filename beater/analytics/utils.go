package analytics

import (
	"fmt"
	"time"
	"unicode"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/textquerytype"
)

// IndexPatternForTodayAndYesterday converts an index pattern like
// log-syslog-informational-* to log-syslog-informational-YYYY-MM-DD for
// both today and yesterday, separated by a comma.
func IndexPatternForTodayAndYesterday(pattern string) string {
	today := time.Now().Format("2006.01.02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006.01.02")
	base := pattern
	if len(pattern) > 0 && pattern[len(pattern)-1] == '*' {
		base = pattern[:len(pattern)-1]
	}
	return fmt.Sprintf("%s%s,%s%s", base, today, base, yesterday)
}

// CreateSchedulerIdMatchQuery creates a query for matching a scheduler ID in the logs.
// If the first character of the UUID is a letter, it creates a bool query with two should clauses to match both the original UUID and the UUID prefixed with "u'".
// If the first character is not a letter, it creates a simple multi-match query for the original UUID.
func CreateSchedulerIdMatchQuery(uuid string) types.Query {
	// Get the first character
	firstChar := rune(uuid[0])

	// If the first character of the UUID is a letter, we need to create a bool query with two should clauses to match both the original UUID and the UUID prefixed with "u'".
	if unicode.IsLetter(firstChar) {
		return types.Query{
			Bool: &types.BoolQuery{
				Should: []types.Query{
					{
						MultiMatch: &types.MultiMatchQuery{
							Query:   uuid,
							Fields:  []string{"log.syslog.message"},
							Type:    &textquerytype.Phrase,
							Lenient: esapi.BoolPtr(true),
						},
					},
					{
						MultiMatch: &types.MultiMatchQuery{
							Query:   "u'" + uuid, // Prefix the UUID with "u'" for the second match
							Fields:  []string{"log.syslog.message"},
							Type:    &textquerytype.Phrase,
							Lenient: esapi.BoolPtr(true),
						},
					},
				},
				MinimumShouldMatch: 1,
			},
		}
	}

	// If the first character of the UUID is not a letter, we can just create a simple multi-match query for the original UUID.
	return types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   uuid,
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	}
}
