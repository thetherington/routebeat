package analytics

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/runtimefieldtype"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/scriptlanguage"
	"github.com/elastic/go-elasticsearch/v8/typedapi/types/enums/sortorder"
)

var fieldMap = map[string]string{
	"pri_src":    "primary_source",
	"sec_src":    "backup_source",
	"start_date": "scheduler.schedule.start_date",
	"end_date":   "scheduler.schedule.end_date",
}

func StringPtr(s string) *string { return &s }

func processBucketsIntoBusMap(buckets []types.StringTermsBucket) BusMap {
	busMap := make(BusMap, 0)

	for _, bucket := range buckets {
		key, ok := bucket.Key.(string)
		if !ok {
			logp.Err("%v", ErrBucketKeyNotString)
			continue
		}

		busMap[key] = &BusRouting{}

		for subKey, agg := range bucket.Aggregations {
			filterAgg, ok := agg.(*types.FilterAggregate)
			if !ok {
				logp.Err("%v, key: %s", ErrCastFilterAggregate, subKey)
				continue
			}

			topAgg, ok := filterAgg.Aggregations["metric"].(*types.TopHitsAggregate)
			if !ok {
				logp.Err("%v, key: %s", ErrCastTopHitAggregate, subKey)
				continue
			}

			// check if the top metrics has atleast one array
			if topAgg.Hits.Total.Value == 0 {
				logp.Err("%v, key: %s, bus: %s", ErrEmptyTopHitValueList, subKey, key)
				continue
			}

			var data []string

			for field, rawMessage := range topAgg.Hits.Hits[0].Fields {
				if err := json.Unmarshal(rawMessage, &data); err != nil {
					logp.Err("%v error: %v", ErrUnmarshallTopHitValue, err)
					continue
				}

				if len(data) == 0 {
					logp.Err("%v, key: %s, field: %s", ErrEmptyTopHitValueList, subKey, field)
					continue
				}

				switch field {
				case "primary_source":
					busMap[key].Pri = data[0]

				case "backup_source":
					busMap[key].Sec = data[0]

				case "scheduler.schedule.start_date":
					busMap[key].StartDate, _ = ParseCustomTime(data[0])

				case "scheduler.schedule.end_date":
					busMap[key].EndDate, _ = ParseCustomTime(data[0])
				}
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
					TopHits: &types.TopHitsAggregation{
						Fields: []types.FieldAndFormat{
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
						Source_: false,
					},
				},
			},
		}
	}

	// root aggregation "bus_name"
	return map[string]types.Aggregations{
		"bus_name": {
			Terms: &types.TermsAggregation{
				Field: StringPtr("buscode"),
				Order: map[string]sortorder.SortOrder{
					"_key": sortorder.Asc,
				},
				Size: esapi.IntPtr(3000),
			},
			Aggregations: subAgg,
		},
	}
}

func createRuntimeMappings() types.RuntimeFields {
	runtimeFields := types.RuntimeFields{
		"buscode": types.RuntimeField{
			Type: runtimefieldtype.Keyword,
			Script: &types.Script{
				Lang: &scriptlanguage.Painless,
				Source: StringPtr(`
					if (doc.containsKey('scheduler.schedule.tags') && 
          				doc['scheduler.schedule.tags'].value.contains(":")
        			) {
          				emit(doc['scheduler.schedule.tags'].value.splitOnToken(":")[1])
        			}
			`),
			},
		},
	}

	script := `
	        if (doc.containsKey('scheduler.schedule.comment') &&
	      		doc['scheduler.schedule.comment'].value.contains("<REPLACE>:")
	    	) {
	      		String message = doc['scheduler.schedule.comment'].value;
	      		Matcher m = /<REPLACE>:\s*(\S+)/.matcher(message);
	      		if (m.find()) {
	        		emit(m.group(1))
	      	}
	    }
	`

	fmap := map[string]string{
		"primary_source": `Source`,
		"backup_source":  `Backup`,
	}

	for k, v := range fmap {
		runtimeFields[k] = types.RuntimeField{
			Type: runtimefieldtype.Keyword,
			Script: &types.Script{
				Lang:   &scriptlanguage.Painless,
				Source: StringPtr(strings.ReplaceAll(script, "<REPLACE>", v)),
			},
		}
	}

	return runtimeFields
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
  "query": {
    "bool": {
      "must": [
        {
          "range": {
            "scheduler.schedule.end_date": {
              "gte": "now"
            }
          }
        },
        {
          "range": {
            "scheduler.schedule.start_date": {
              "lte": "now"
            }
          }
        },
        {
          "range": {
            "@timestamp": {
              "gte": "now-4h",
              "lte": "now"
            }
          }
        },
        {
          "match_phrase": {
            "event.module": "schedule"
          }
        }
      ]
    }
  },
  "runtime_mappings": {
    "buscode": {
      "type": "keyword",
      "script": """
        if (doc.containsKey('scheduler.schedule.tags') &&
          doc['scheduler.schedule.tags'].value.contains(":")
        ) {
          emit(doc['scheduler.schedule.tags'].value.splitOnToken(":")[1])
        }
      """
    },
    "primary_source": {
      "type": "keyword",
      "script": """
        if (doc.containsKey('scheduler.schedule.comment') &&
          doc['scheduler.schedule.comment'].value.contains("Source:")
        ) {
          String message = doc['scheduler.schedule.comment'].value;
          Matcher m = /Source:\s*(\S+)/.matcher(message);
          if (m.find()) {
            emit(m.group(1))
          }
        }
      """
    },
    "backup_source": {
      "type": "keyword",
      "script": """
        if (doc.containsKey('scheduler.schedule.comment') &&
          doc['scheduler.schedule.comment'].value.contains("Backup:")
        ) {
          String message = doc['scheduler.schedule.comment'].value;
          Matcher m = /Backup:\s*(\S+)/.matcher(message);
          if (m.find()) {
            emit(m.group(1))
          }
        }
      """
    }
  },
  "aggs": {
    "bus_codes": {
      "terms": {
        "field": "buscode",
        "order": {
          "_key": "asc"
        },
        "size": 3000
      },
      "aggs": {
        "pri_src": {
          "filter": {
            "bool": {
              "filter": [
                {
                  "bool": {
                    "should": [
                      {
                        "exists": {
                          "field": "primary_source"
                        }
                      }
                    ],
                    "minimum_should_match": 1
                  }
                }
              ]
            }
          },
          "aggs": {
            "metric": {
              "top_hits": {
                "fields": [
                  {
                    "field": "primary_source"
                  }
                ],
                "_source": false,
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
          }
        },
        "sec_src": {
          "filter": {
            "bool": {
              "filter": [
                {
                  "bool": {
                    "should": [
                      {
                        "exists": {
                          "field": "backup_source"
                        }
                      }
                    ],
                    "minimum_should_match": 1
                  }
                }
              ]
            }
          },
          "aggs": {
            "metric": {
              "top_hits": {
                "fields": [
                  {
                    "field": "backup_source"
                  }
                ],
                "_source": false,
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
          }
        }
      }
    }
  },
  "size": 0
}
*/
