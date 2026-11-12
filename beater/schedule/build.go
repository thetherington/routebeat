package schedule

import "errors"

// BuildRoutes groups ungrouped events by output into routing windows sorted by start date.
// MAIN and BACKUP events with an identical start and end are folded into one routing.
// events without a MAIN/BACKUP tag or with a bad date are counted and skipped
func BuildRoutes(events []Event) (map[string][]*BusRouting, Stats) {
	var stats Stats

	routes := make(map[string][]*BusRouting)

	for i := range events {
		event := &events[i]

		br, err := event.Routing()
		if err != nil {
			if errors.Is(err, ErrNoTag) {
				stats.NoTag++
			} else {
				stats.Invalid++
			}

			continue
		}

		routes[event.Output] = foldWindow(routes[event.Output], br)
	}

	for output, list := range routes {
		sortByStart(list)
		stats.Routings += len(list)
		routes[output] = list
	}

	return routes, stats
}

// MergeRouteLists combines routing lists by output. exact duplicate windows are folded
// together, nil routings and routings without a window are dropped, and each list is sorted
func MergeRouteLists(lists ...map[string][]*BusRouting) map[string][]*BusRouting {
	merged := make(map[string][]*BusRouting)

	for _, list := range lists {
		for output, routes := range list {
			for _, r := range routes {
				if !hasWindow(r) {
					continue
				}

				merged[output] = foldWindow(merged[output], r)
			}
		}
	}

	for _, routes := range merged {
		sortByStart(routes)
	}

	return merged
}

// foldWindow merges r into the routing with the identical window, or appends a copy of it
func foldWindow(routes []*BusRouting, r *BusRouting) []*BusRouting {
	for i, existing := range routes {
		if sameWindow(existing, r) {
			routes[i] = MergeBusRouting(existing, r)
			return routes
		}
	}

	c := *r

	return append(routes, &c)
}
