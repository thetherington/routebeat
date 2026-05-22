package analytics

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/core/search"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/textquerytype"
)

func (es *ESSearch) SearchMagnumLogs(source string, destination string) ([]Source, error) {
	req := &search.Request{
		Query: createMagnumLogsQuery(source, destination),
		Sort: []types.SortCombinations{
			types.SortOptions{
				SortOptions: map[string]types.FieldSort{
					"@timestamp": {Order: &sortorder.Asc},
				},
			},
		},
		Source_: types.SourceFilter{
			Includes: []string{"log.syslog.message", "device.timestamp"},
		},
		Size: esapi.IntPtr(3000),
	}

	index := es.index
	if !es.Strict {
		// If strict is false, use the IndexPatternForTodayAndYesterday function to get the index pattern for today and yesterday
		index = IndexPatternForTodayAndYesterday(es.index)
	}

	resp, err := es.client.Search().Index(index).Request(req).IgnoreUnavailable(true).Do(context.Background())

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchFailed, err)
	}

	var results []Source
	for _, hit := range resp.Hits.Hits {
		var src Source

		src.Id = *hit.Id_
		if err := json.Unmarshal(hit.Source_, &src); err != nil {
			return nil, fmt.Errorf("failed to unmarshal hit: %w", err)
		}

		results = append(results, src)
	}

	if len(results) < 1 {
		return nil, ErrNoResults
	}

	return results, nil
}

func createMagnumLogsQuery(source string, destination string) *types.Query {
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

	// filter for the magrtrsrv process
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MatchPhrase: map[string]types.MatchPhraseQuery{
			"process.name": {Query: PROCESS},
		},
	})

	// sub bool with a should for using two multi match queries for "INFO:jsonrpc:Subscribe request" or "INFO:jsonrpc:Route Complete"
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Bool: &types.BoolQuery{
			Should: []types.Query{
				{
					MultiMatch: &types.MultiMatchQuery{
						Query:  REQUEST_LOGS,
						Fields: []string{"log.syslog.message"},
						Type:   &textquerytype.Phrase,
					},
				},
				{
					MultiMatch: &types.MultiMatchQuery{
						Query:  COMPLETION_LOGS,
						Fields: []string{"log.syslog.message"},
						Type:   &textquerytype.Phrase,
					},
				},
			},
		},
	})

	// match source and destination using multi_match type phrase queries
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   source,
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		MultiMatch: &types.MultiMatchQuery{
			Query:   destination,
			Fields:  []string{"log.syslog.message"},
			Type:    &textquerytype.Phrase,
			Lenient: esapi.BoolPtr(true),
		},
	})

	return &types.Query{
		Bool: &types.BoolQuery{
			Must: mustBoolSlice,
		},
	}
}
