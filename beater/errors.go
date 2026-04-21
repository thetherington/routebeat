package beater

import "errors"

var (
	ErrNoLogsFound     = errors.New("no logs found for event")
	ErrNoRequestLogs   = errors.New("no request logs found for event")
	ErrNoCompletedLogs = errors.New("no completed logs found for event")
	ErrOrphanedRequest = errors.New("orphaned request log found for event")
)
