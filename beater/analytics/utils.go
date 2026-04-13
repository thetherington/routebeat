package analytics

import (
	"fmt"
	"time"
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
