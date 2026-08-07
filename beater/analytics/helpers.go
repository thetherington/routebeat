package analytics

import (
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
)

var datesFieldMap = map[string]string{
	"start_date": "scheduler.event.start_date",
	"end_date":   "scheduler.event.end_date",
}

var sourceFieldMap = map[string]string{
	"input": "scheduler.event.input",
}

func StringPtr(s string) *string { return &s }

func processBucketsIntoBusMap(buckets []types.StringTermsBucket) BusRouteMap {
	busMap := make(BusRouteMap, 0)

	// [{ "key": "BSBRDG", "doc_count": 104, "end_date": {}, "source": {}, "start_date": {}}]
	for _, bucket := range buckets {
		key, ok := bucket.Key.(string)
		if !ok || key == "" {
			logp.Err("%v", ErrBucketKeyNotString)
			continue
		}

		busMap[key] = &BusRouting{}

		for subKey, agg := range bucket.Aggregations {

			// check if the sub aggregation is for the source field (input) or the date fields (start_date, end_date)
			// for the source field, loop through the buckets of the terms aggregation and extract the top metric value for
			// the input field for each source (MAIN / BACKUP)
			if subKey == "source" {
				/*
					"source": {
						"doc_count_error_upper_bound": 0, "sum_other_doc_count": 0,
						"buckets": [ { "key": "BACKUP", "doc_count": 52, "input": {}},
							{ "key": "MAIN", "doc_count": 52, "input": {}}]
				*/
				// cast the source aggregation into a StringTermsAggregate to access the buckets
				stringTermsAgg, ok := agg.(*types.StringTermsAggregate)
				if !ok {
					logp.Err("%v, key: %s", ErrCastStringTermsBucket, subKey)
					continue
				}

				buckets, ok := stringTermsAgg.Buckets.([]types.StringTermsBucket)
				if !ok {
					logp.Err("%v, key: %s", ErrCastStringTermsBucket, subKey)
					continue
				}

				if len(buckets) == 0 {
					logp.Err("%v, key: %s", ErrZeroBuckets, subKey)
					continue
				}

				// loop through the source buckets (MAIN / BACKUP) and extract the top metric value for the input field
				for _, bucket := range buckets {
					inputKey, ok := bucket.Key.(string)
					if !ok || inputKey == "" {
						logp.Err("%v", ErrBucketKeyNotString)
						continue
					}

					// ignore any tag term that's not MAIN or BACKUP
					if inputKey == "MAIN" || inputKey == "BACKUP" {
						/*
							"input": {
							  "doc_count": 52,
							  "metric": {
							    "top": [{"sort": ["2026-05-24T23:00:16.454Z"],"metrics": {"scheduler.event.input": "ATZA002B"}}]
							  }
						*/
						// the input field sub aggregation has a filter aggregation -> top metrics aggregation structure,
						// so we need to cast twice to access the top metric value
						inputFilterAgg, ok := bucket.Aggregations["input"].(*types.FilterAggregate)
						if !ok {
							logp.Err("%v, key: %s", ErrCastFilterAggregate, subKey)
							continue
						}

						topAgg, ok := inputFilterAgg.Aggregations["metric"].(*types.TopMetricsAggregate)
						if !ok {
							logp.Err("%v, key: %s", ErrCastTopMetricsAggregate, subKey)
							continue
						}

						if len(topAgg.Top) == 0 {
							continue
						}

						// extract the value of the input field from the top metrics aggregation
						// and set it in the busMap for the corresponding bus and source (MAIN / BACKUP)
						if v, ok := topAgg.Top[0].Metrics[sourceFieldMap["input"]]; ok {
							switch inputKey {
							case "MAIN":
								busMap[key].Pri = v.(string)
							case "BACKUP":
								busMap[key].Sec = v.(string)
							}
						}
					}
				}
			}

			// for the date fields, extract the top metric value and set it in the busMap for the corresponding bus
			// the date field sub aggregations have a filter aggregation -> top metrics aggregation structure, so we need to cast twice to access the top metric value
			if subKey == "start_date" || subKey == "end_date" {
				/*
					  "start_date": {
						"doc_count": 104,
						"metric": {
							"top": [{"sort": ["2026-05-24T23:00:16.454Z"],"metrics": {"scheduler.event.start_date": "2026/05/24 04:00:00"}}]
				*/
				// cast the sub aggregation into a FilterAggregate to access the nested top metrics aggregation
				filterAgg, ok := agg.(*types.FilterAggregate)
				if !ok {
					logp.Err("%v, key: %s", ErrCastFilterAggregate, subKey)
					continue
				}

				topAgg, ok := filterAgg.Aggregations["metric"].(*types.TopMetricsAggregate)
				if !ok {
					logp.Err("%v, key: %s", ErrCastTopMetricsAggregate, subKey)
					continue
				}

				if len(topAgg.Top) == 0 {
					continue
				}

				// extract the value of the date field from the top metrics aggregation and
				// set it in the busMap for the corresponding bus
				if v, ok := topAgg.Top[0].Metrics[datesFieldMap[subKey]]; ok {
					switch subKey {
					case "start_date":
						busMap[key].StartDate, _ = ParseCustomTime(v.(string))
					case "end_date":
						busMap[key].EndDate, _ = ParseCustomTime(v.(string))
					}
				}
			}
		}
	}

	return busMap
}

func createQuery(relative_time string) *types.Query {
	mustBoolSlice := make([]types.Query, 0)

	// filter for events in the last hour
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"@timestamp": types.DateRangeQuery{
				Gte: StringPtr("now-4h"),
				Lte: StringPtr("now"),
			},
		},
	})

	// filter for the schedule module
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MatchPhrase: map[string]types.MatchPhraseQuery{
			"event.module": {Query: "event"},
		},
	})

	// filter for the apg task
	mustBoolSlice = append(mustBoolSlice, types.Query{
		MatchPhrase: map[string]types.MatchPhraseQuery{
			"scheduler.event.task": {Query: "apg"},
		},
	})

	// filter for current event time range from start_time / end_time
	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"scheduler.event.end_date": types.DateRangeQuery{
				Gte: StringPtr(relative_time),
			},
		},
	})

	mustBoolSlice = append(mustBoolSlice, types.Query{
		Range: map[string]types.RangeQuery{
			"scheduler.event.start_date": types.DateRangeQuery{
				Lte: StringPtr(relative_time),
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
	// sub aggregation for the start and end dates and the input terms aggregations for MAIN / BACKUP tags
	busAggs := make(map[string]types.Aggregations, 0)

	// loop through the dates field map and create filter -> top metric aggs for each date field (start_date and end_date)
	for key, fieldName := range datesFieldMap {
		busAggs[key] = types.Aggregations{
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

	// loop through the source field map and create filter -> top metric aggs. currently just input
	eventTagAgg := make(map[string]types.Aggregations, 0)

	for key, fieldName := range sourceFieldMap {
		eventTagAgg[key] = types.Aggregations{
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

	// sub aggregation for the source of the event (input MAIN / BACKUP)
	busAggs["source"] = types.Aggregations{
		Terms: &types.TermsAggregation{
			Field: StringPtr("scheduler.event.tags"),
			Order: map[string]sortorder.SortOrder{
				"_key": sortorder.Asc,
			},
			Size: esapi.IntPtr(10),
		},
		Aggregations: eventTagAgg,
	}

	// root aggregation "bus_name" with the busAggs as the sub aggregations
	return map[string]types.Aggregations{
		"bus_name": {
			Terms: &types.TermsAggregation{
				Field: StringPtr("scheduler.event.output"),
				Order: map[string]sortorder.SortOrder{
					"_key": sortorder.Asc,
				},
				Size: esapi.IntPtr(3000),
			},
			Aggregations: busAggs,
		},
	}
}

// ParseCustomTime takes a string in "2006/01/02 15:04:05" format and returns a pointer to a time.Time
func ParseCustomTime(input string) (*time.Time, error) {
	layout := "2006/01/02 15:04:05"
	t, err := time.Parse(layout, input)
	if err != nil {
		return nil, err
	}

	return &t, nil
}

/*
GET log-magnum-scheduler-./_search
{
    "aggregations": {
        "bus_name": {
            "aggregations": {
                "end_date": {
                    "aggregations": {
                        "metric": {
                            "top_metrics": {
                                "metrics": [
                                    {
                                        "field": "scheduler.event.end_date"
                                    }
                                ],
                                "size": 1,
                                "sort": [
                                    {
                                        "@timestamp": {
                                            "order": "desc"
                                        }
                                    }
                                ]
                            }
                        }
                    },
                    "filter": {
                        "bool": {
                            "minimum_should_match": 1,
                            "should": [
                                {
                                    "exists": {
                                        "field": "scheduler.event.end_date"
                                    }
                                }
                            ]
                        }
                    }
                },
                "source": {
                    "aggregations": {
                        "input": {
                            "aggregations": {
                                "metric": {
                                    "top_metrics": {
                                        "metrics": [
                                            {
                                                "field": "scheduler.event.input"
                                            }
                                        ],
                                        "size": 1,
                                        "sort": [
                                            {
                                                "@timestamp": {
                                                    "order": "desc"
                                                }
                                            }
                                        ]
                                    }
                                }
                            },
                            "filter": {
                                "bool": {
                                    "minimum_should_match": 1,
                                    "should": [
                                        {
                                            "exists": {
                                                "field": "scheduler.event.input"
                                            }
                                        }
                                    ]
                                }
                            }
                        }
                    },
                    "terms": {
                        "field": "scheduler.event.tags",
                        "order": {
                            "_key": "asc"
                        },
                        "size": 10
                    }
                },
                "start_date": {
                    "aggregations": {
                        "metric": {
                            "top_metrics": {
                                "metrics": [
                                    {
                                        "field": "scheduler.event.start_date"
                                    }
                                ],
                                "size": 1,
                                "sort": [
                                    {
                                        "@timestamp": {
                                            "order": "desc"
                                        }
                                    }
                                ]
                            }
                        }
                    },
                    "filter": {
                        "bool": {
                            "minimum_should_match": 1,
                            "should": [
                                {
                                    "exists": {
                                        "field": "scheduler.event.start_date"
                                    }
                                }
                            ]
                        }
                    }
                }
            },
            "terms": {
                "field": "scheduler.event.output",
                "order": {
                    "_key": "asc"
                },
                "size": 3000
            }
        }
    },
    "query": {
        "bool": {
            "must": [
                {
                    "range": {
                        "@timestamp": {
                            "from": "now-4h",
                            "to": "now"
                        }
                    }
                },
                {
                    "match_phrase": {
                        "event.module": {
                            "query": "event"
                        }
                    }
                },
                {
                    "match_phrase": {
                        "scheduler.event.task": {
                            "query": "apg"
                        }
                    }
                },
                {
                    "range": {
                        "scheduler.event.end_date": {
                            "gte": "now"
                        }
                    }
                },
                {
                    "range": {
                        "scheduler.event.start_date": {
                            "lte": "now"
                        }
                    }
                }
            ]
        }
    },
    "size": 0
}
*/
