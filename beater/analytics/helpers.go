package analytics

import (
	"fmt"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
)

var fieldMap = map[string]string{
	"pri_src": "scheduler.schedule.events.event_params.pri_src",
	"sec_src": "scheduler.schedule.events.event_params.sec_src",
}

func StringPtr(s string) *string { return &s }

func processBucketsIntoBusMap(buckets []types.StringTermsBucket) BusMap {
	busMap := make(BusMap, 0)

	for _, bucket := range buckets {
		key, ok := bucket.Key.(string)
		if !ok {
			fmt.Println(ErrBucketKeyNotString)
			continue
		}

		busMap[key] = &BusRouting{}

		for subKey, agg := range bucket.Aggregations {
			filterAgg, ok := agg.(*types.FilterAggregate)
			if !ok {
				fmt.Println(ErrCastFilterAggregate, subKey)
				continue
			}

			topAgg, ok := filterAgg.Aggregations["metric"].(*types.TopMetricsAggregate)
			if !ok {
				fmt.Println(ErrCastTopMetricsAggregate, subKey)
				continue
			}

			// check if the top metrics has atleast one array
			if len(topAgg.Top) == 0 {
				continue
			}

			if v, ok := topAgg.Top[0].Metrics[fieldMap["pri_src"]]; ok {
				busMap[key].Pri = v.(string)
			}

			if v, ok := topAgg.Top[0].Metrics[fieldMap["sec_src"]]; ok {
				busMap[key].Sec = v.(string)
			}
		}
	}

	return busMap
}

func createQuery() *types.Query {
	mustBoolSlice := make([]types.Query, 0)

	// filter for events in the last hour
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"@timestamp": types.DateRangeQuery{
				From: StringPtr("now-4h"),
				To:   StringPtr("now"),
			},
		},
	})

	// filter for the schedule module
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MatchPhrase: map[string]types.MatchPhraseQuery{
			"event.module": {Query: "schedule"},
		},
	})

	// filter for current event time range from start_time / end_time
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"scheduler.schedule.end_date": types.DateRangeQuery{
				Gte: StringPtr("now"),
			},
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"scheduler.schedule.start_date": types.DateRangeQuery{
				Lte: StringPtr("now"),
			},
		},
	})

	// return the Bool query
	return &types.Query{
		Bool: &types.BoolQuery{
			Must: mustBoolSlice,
		},
	}
}

func createAggregations() map[string]types.Aggregations {

	// sub aggregation Top Metrics with filter expression
	subAgg := make(map[string]types.Aggregations, 0)

	for key, fieldName := range fieldMap {
		subAgg[key] = types.Aggregations{
			Filter: &types.Query{
				Bool: &types.BoolQuery{
					Should: []types.Query{
						{Exists: &types.ExistsQuery{Field: fieldName}},
					},
					MinimumShouldMatch: 1,
				},
			},
			Aggregations: map[string]types.Aggregations{
				"metric": {
					TopMetrics: &types.TopMetricsAggregation{
						Metrics: []types.TopMetricsValue{
							{Field: fieldName},
						},
						Size: esapi.IntPtr(1),
						Sort: []types.SortCombinations{
							&types.SortOptions{
								SortOptions: map[string]types.FieldSort{
									"@timestamp": {
										Order: &sortorder.Desc,
									},
								},
							},
						},
					},
				},
			},
		}
	}

	// root aggregation "bus_name"
	return map[string]types.Aggregations{
		"bus_name": {
			Terms: &types.TermsAggregation{
				Field: StringPtr("scheduler.schedule.events.event_params.bus_name"),
				Order: map[string]sortorder.SortOrder{
					"_key": sortorder.Asc,
				},
				Size: esapi.IntPtr(2000),
			},
			Aggregations: subAgg,
		},
	}
}
