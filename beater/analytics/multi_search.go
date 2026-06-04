package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/textquerytype"
)

const (
	SlabLogsIndex      = "log-syslog-informational-*"
	SchedulerLogsIndex = "log-syslog-debug-*"
)

// MultiLogQuery is the sealed interface for variadic multi-search query configs.
// Use NewSlabLogQuery or NewSchedulerLogQuery to construct instances.
type MultiLogQuery interface {
	// Key returns the unique identifier used to key results in the response map.
	Key() string
	indexPattern() string
	buildQuery() *types.Query
}

// SlabLogConfig is the configuration builder for slab log queries.
// Index pattern: log-syslog-informational-*
type SlabLogConfig struct {
	Slab      string
	Multicast string
	Output    int
}

// NewSlabLogQuery returns a MultiLogQuery that searches the slab informational log index.
func NewSlabLogQuery(slab, multicast string, output int) MultiLogQuery {
	return &SlabLogConfig{Slab: slab, Multicast: multicast, Output: output}
}

func (c *SlabLogConfig) Key() string          { return "slab:" + c.Slab }
func (c *SlabLogConfig) indexPattern() string { return SlabLogsIndex }
func (c *SlabLogConfig) buildQuery() *types.Query {
	return createSlabLogsQuery(c.Slab, c.Multicast, c.Output)
}

// SchedulerLogConfig is the configuration builder for scheduler log queries.
// Index pattern: log-syslog-debug-*
type SchedulerLogConfig struct {
	Src string
	Dst string
}

// NewSchedulerLogQuery returns a MultiLogQuery that searches the scheduler debug log index.
func NewSchedulerLogQuery(src, dst string) MultiLogQuery {
	return &SchedulerLogConfig{Src: src, Dst: dst}
}

func (c *SchedulerLogConfig) Key() string          { return "scheduler:" + c.Src + "->" + c.Dst }
func (c *SchedulerLogConfig) indexPattern() string { return SchedulerLogsIndex }
func (c *SchedulerLogConfig) buildQuery() *types.Query {
	return createSchedulerLogsQuery(c.Src, c.Dst)
}

// SearchMultiLogs performs a single multi-search (msearch) request containing one
// sub-query per provided MultiLogQuery config. It returns a map of []Source keyed
// by each query's Key(). Queries with no results are omitted from the map.
func (es *ESSearch) SearchMultiLogs(ctx context.Context, queries ...MultiLogQuery) (MultiLogResults, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("%w: at least one query config is required", ErrInvalidConfig)
	}

	// Build the msearch request by iterating over the provided query configs and adding
	// a search for each one to the request body.
	ms := es.client.Msearch()

	for _, q := range queries {
		index := q.indexPattern()
		if !es.Strict {
			index = IndexPatternForTodayAndYesterday(index)
		}

		header := types.MultisearchHeader{
			Index:             strings.Split(index, ","),
			IgnoreUnavailable: esapi.BoolPtr(true),
		}

		body := types.MultisearchBody{
			Query: q.buildQuery(),
			Sort: []types.SortCombinations{
				types.SortOptions{
					SortOptions: map[string]types.FieldSort{
						"@timestamp": {Order: &sortorder.Asc},
					},
				},
			},
			Source_: &types.SourceFilter{
				Includes: []string{"@timestamp", "log.syslog.message", "device.timestamp", "annotation.general.device_name"},
			},
			Size: esapi.IntPtr(3000),
		}

		if err := ms.AddSearch(header, body); err != nil {
			return nil, fmt.Errorf("failed to build msearch request for %q: %w", q.Key(), err)
		}
	}

	// Execute the msearch request and parse the response, mapping each query's Key() to its []Source results.
	resp, err := ms.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchFailed, err)
	}

	if len(resp.Responses) != len(queries) {
		return nil, fmt.Errorf("%w: expected %d responses, got %d", ErrSearchFailed, len(queries), len(resp.Responses))
	}

	results := make(MultiLogResults)

	// Iterate over the msearch responses, unmarshalling hits into Source structs and mapping them to their query's Key().
	for i, raw := range resp.Responses {
		key := queries[i].Key()

		item, ok := raw.(*types.MultiSearchItem)
		if !ok {
			if errResp, isErr := raw.(*types.ErrorResponseBase); isErr {
				logp.Warn("SearchMultiLogs: msearch error for %q: type=%s reason=%s", key, errResp.Error.Type, *errResp.Error.Reason)
			}
			continue
		}

		var sources []Source
		for _, hit := range item.Hits.Hits {
			var src Source
			src.Id = *hit.Id_
			if err := json.Unmarshal(hit.Source_, &src); err != nil {
				return nil, fmt.Errorf("failed to unmarshal hit for %q: %w", key, err)
			}

			switch queries[i].(type) {
			// Slab logs don't have a timezone in their syslog timestamps, so we assume they're in EST/EDT
			// and then convert them to properly to UTC so they can be accurately compared to the magnum log timestamps.
			// Scheduler logs have UTC timestamps, so no adjustment is needed.
			// Future: possibly detect timezone based on current time and adjust accordingly
			case *SlabLogConfig:
				if es.TimeZoneFix {
					loc, _ := time.LoadLocation("America/New_York") // handles EST/EDT
					t := src.Device.Timestamp
					fixed := time.Date(
						t.Year(), t.Month(), t.Day(),
						t.Hour(), t.Minute(), t.Second(), t.Nanosecond(),
						loc,
					)
					src.Device.Timestamp = fixed.UTC()
				}

				if es.UseLogIngestTimestamp {
					src.Device.Timestamp = src.Timestamp
				}
			case *SchedulerLogConfig:
				// future: custom adjustments for scheduler log hits
			}

			sources = append(sources, src)
		}

		if len(sources) > 0 {
			results[key] = sources
		}
	}

	if len(results) == 0 {
		return nil, ErrNoResults
	}

	return results, nil
}

func createSlabLogsQuery(slab, multicast string, output int) *types.Query {
	mustBoolSlice := make([]types.Query, 0)

	// filter for events in the 30 minute window
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"@timestamp": types.DateRangeQuery{
				From: StringPtr(FROM),
				To:   StringPtr("now"),
			},
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   fmt.Sprintf("AuditSet:DST %d", output),
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   multicast,
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	// filter for slab by annotation.name
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MatchPhrase: map[string]types.MatchPhraseQuery{
			"annotation.general.device_name": {Query: slab},
		},
	})

	return &types.Query{
		Bool: &types.BoolQuery{
			Must: mustBoolSlice,
		},
	}
}

func createSchedulerLogsQuery(src, dst string) *types.Query {
	mustBoolSlice := make([]types.Query, 0)

	// filter for events in the 30 minute window
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"@timestamp": types.DateRangeQuery{
				From: StringPtr(FROM),
				To:   StringPtr("now"),
			},
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   "dcpipes.jsonrpctcp: SENDING",
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   "route",
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	// Add queries for src and dst, handling the case where the first character of the UUID is a letter.
	mustBoolSlice = append(mustBoolSlice, CreateSchedulerIdMatchQuery(src))
	mustBoolSlice = append(mustBoolSlice, CreateSchedulerIdMatchQuery(dst))

	return &types.Query{
		Bool: &types.BoolQuery{
			Must: mustBoolSlice,
		},
	}
}
