# Route Deviation Tests

This document describes the unit tests that cover routebeat's route deviation logic: what each test sets up, what it checks, and why. For how the logic itself works, see the [README](../README.md).

Run them all with the race detector:

```bash
go test -race -count=1 ./beater/... ./config/
```

Run one group, for example:

```bash
go test -count=1 -run 'TestCreateEventFromEdgeUnscheduledRecheck' -v ./beater/
```

## Contents

- [How the tests are built](#how-the-tests-are-built)
- [Coverage map](#coverage-map)
- [Event building and labels](#event-building-and-labels)
- [Schedule lookup](#schedule-lookup)
- [Routing state resolution](#routing-state-resolution)
- [Blank source (unknown state)](#blank-source-unknown-state)
- [Notifications with no state change](#notifications-with-no-state-change)
- [State transitions and alerts](#state-transitions-and-alerts)
- [Unscheduled recheck](#unscheduled-recheck)
- [Self-correction (defunct state)](#self-correction-defunct-state)
- [Multi-step sequence](#multi-step-sequence)
- [Notifier fallback](#notifier-fallback)
- [Schedule poll and refresh](#schedule-poll-and-refresh)
- [Schedule package](#schedule-package)
- [Real lab data replays](#real-lab-data-replays)
- [Supporting packages](#supporting-packages)
- [Known behaviour pinned by the tests](#known-behaviour-pinned-by-the-tests)

---

## How the tests are built

The tests call the deviation engine directly with Magnum SDVN destinations built in memory, so they need no Magnum, events API or notification server.

| Building block | Purpose |
|---|---|
| **Test destination** | a destination `BUS1` on tag `MES`, with a Broadview nameset. Options control its tags, its subscribed source label and source tags, and its routed physical source. |
| **Seeded schedule** | an active schedule window for `BUS1` with primary `SRC1` and backup `SRC2`, as if it came from the events API poll |
| **Seeded bus state** | a stored state and source for the bus, with deviation times set minutes in the past so each time can be told apart from "now" |
| **Stub notifier** | captures alerts instead of sending them |
| **Stub events API** | a local HTTP server that returns a chosen scheduler payload or error status and counts how many calls it received |
| **No-wait bound** | 50ms: a call that shouldn't wait on a recheck must return faster than this |

Each test starts from an empty schedule store and bus cache. Times are compared exactly when they're seeded, and as "within 2 seconds of now" when routebeat sets them.

Each case can check any of the following:
- the `schedule.*` fields (status, routing source, primary, backup, matched);
- every alert: whether it's `previous` or `current`, Error or Correction, its status, source, start and end;
- the counter changes for the tag;
- the stored bus state and source afterwards;
- whether the unscheduled recheck returned without waiting, how many events API calls it made, and whether the schedule store changed.

## Coverage map

| Area | Tests |
|---|---|
| Event fields, namesets, labels | `TestCreateEventFromEdgeBuild` |
| Schedule lookup | `TestCreateEventFromEdgeScheduleLookup`, `TestGetBusRouting` |
| Primary / Backup / Zorro / TDA / Unscheduled | `TestCreateEventFromEdgeStateResolution`, `TestMatchScheduledState`, `TestResolveRoutingState` |
| Unknown (blank source) | `TestCreateEventFromEdgeBlankSource` |
| Transitions, alerts, counters, deviation times | `TestCreateEventFromEdgeTransitions`, `TestCreateEventFromEdgeNotificationNoChange`, `TestCreateEventFromEdgeSequence` |
| Unscheduled recheck | `TestCreateEventFromEdgeUnscheduledRecheck`, `TestRecheckRoute`, `TestRecheckRouteLogs`, `TestIsUnscheduledTransition`, `TestRefreshOutput`, `TestCreateEventFromEdgeConcurrentRecheck` |
| Self-correction | `TestCreateEventFromEdgeQueryDefunct` |
| Schedule poll and persistence | `TestPollSchedule`, `TestRelativeTime`, `TestSaveCaches` |
| Scheduler payloads, routing windows, schedule store | `schedule` package: `TestParseEvents`, `TestEventRouting`, `TestBuildRoutes`, `TestMergeRouteLists`, `TestMergeBusRouting`, `TestStore*` |
| Real lab data | `TestRealEvents*`, `TestBuildRoutesRealPair` (`schedule` package), `TestRealRoutebeatReplay`, `TestRealNotificationSequence` |

All statements in the deviation engine and the unscheduled recheck are covered.

---

## Event building and labels

**`TestCreateEventFromEdgeBuild`** checks how a Magnum SDVN destination becomes an event, and which labels are compared with the scheduler.

| Case | What it checks |
|---|---|
| tag must match exactly | a destination tagged `MES-2` returned for tag `MES` is discarded (the Magnum tag filter can match partially) |
| base fields | `dstId`, `dstName`, `dstTag`, `dstTags`, `eventType` and `dstNameset.broadview` come from the destination |
| mapping disabled | without a mapping nameset there's no `destinationLabel` or `sourceLabel`, and the bus isn't found in the schedule |
| destination nameset missing uses the default label | a destination without the mapping nameset gets `mapping.default` as its label |
| physical source only | the routed physical source is published under `routeableTerminal.physicalSource` and gives the `sourceLabel` |
| subscribed source overrides the physical source label | when both exist, `sourceLabel` comes from the subscribed source |
| no sources removes routeableTerminal | a destination with no sources has no `routeableTerminal` field |

## Schedule lookup

**`TestCreateEventFromEdgeScheduleLookup`** checks where the schedule routing comes from, as reported by `schedule.routingSource`.

| Case | Expected |
|---|---|
| not found | `matched: false`, `routingSource: none`, no counters, no bus state created |
| expired entry with end date validation | not found |
| expired entry without end date validation | found, `routingSource: cache` (development setting) |
| active entry | `routingSource: cache` with the stored primary and backup |

**`TestGetBusRouting`** tests the same rules on the lookup alone: active, not found, and expired with and without end date validation.

## Routing state resolution

**`TestCreateEventFromEdgeStateResolution`** runs a query for a bus seen for the first time. For each case it checks the status, a +1 on that state's counter, the stored bus state, and that no alerts are sent.

| Case | Source | Status |
|---|---|---|
| primary | `SRC1` (= scheduled primary) | `Primary` |
| backup | `SRC2` (= scheduled backup) | `Backup` |
| zorro | unscheduled source with the Zorro tag | `Zorro` |
| tda | unscheduled source with the TDA tag | `TDA` |
| unscheduled | unscheduled source, no fallback tag | `UnscheduledAudio` |
| label match beats source tag | `SRC1` that also has the Zorro tag | `Primary` |

**`TestMatchScheduledState`** checks the label comparison: primary, backup, no match, and that a **blank label never matches a blank primary or backup**.

**`TestResolveRoutingState`** checks the order of the rules (label first, then Zorro, then TDA), and that a blank label is `unknown` whatever tags the source has.

## Blank source (unknown state)

**`TestCreateEventFromEdgeBlankSource`** checks that a destination with no usable source label is reported as `unknown` and has no other effect. Every case checks for `schedule.status: unknown`, no deviation times, no alerts, no counters, and an unchanged bus state.

| Case | What it covers |
|---|---|
| query on a new bus | no bus state is created |
| query keeps the stored state | an existing Backup state isn't touched |
| notification keeps the stored state | a notification doesn't cause a transition |
| physical source only | no subscribed source means no state label |
| subscribed source without a Broadview nameset | the default label is blank, so the state is unknown |
| zorro tag does not apply without a label | fallback tags don't turn a blank label into `Zorro` |
| no recheck with the events api enabled | no wait, no events API call |

This replaces an older behaviour in which a blank source matched a blank schedule backup and was reported as `Backup`. That behaviour was found in real lab data.

## Notifications with no state change

**`TestCreateEventFromEdgeNotificationNoChange`**

| Case | Expected |
|---|---|
| same state updates the source label | Primary → Primary on a new source: the stored source is updated, `deviationStartTime: -`, no alerts |
| same state reports the existing transition time | Backup → Backup: `deviationStartTime` is the seeded transition time |
| unscheduled to unscheduled does not recheck | no wait and no events API call, even with the events API configured |

## State transitions and alerts

**`TestCreateEventFromEdgeTransitions`** sends a notification that changes the state. Each case checks the alerts, the counter changes, the stored bus state, and both deviation times.

| Case | Alerts | Counters | Deviation times |
|---|---|---|---|
| new bus to backup | `current` Backup (Error) | Backup +1 | start now |
| new bus to primary | `current` Primary (Correction) | Primary +1 | n/a |
| primary to backup | `previous` Primary + `current` Backup (Error) | Primary −1, Backup +1 | start now |
| primary to backup after a previous restore | `previous` starts at the restore time | Primary −1, Backup +1 | start now; the first departure is recorded |
| primary to zorro | `previous` Primary + `current` Zorro (Error) | Primary −1, Zorro +1 | start now |
| primary to tda | `previous` Primary + `current` TDA (Error) | Primary −1, TDA +1 | start now |
| backup to primary reports when the bus first left primary | `previous` Backup + `current` Primary (Correction) | Backup −1, Primary +1 | start = first departure, end = now; deviation cleared |
| backup to primary on the first deviation reports the last transition | as above | as above | start = last transition (see [known behaviour](#known-behaviour-pinned-by-the-tests)) |
| primary to unscheduled without the events api | `previous` Primary + `current` UnscheduledAudio (Error) | Primary −1, Unscheduled +1 | no recheck wait |
| new bus to unscheduled does not recheck | `current` UnscheduledAudio | Unscheduled +1 | no wait, no events API call |

## Unscheduled recheck

**`TestCreateEventFromEdgeUnscheduledRecheck`** sends a notification that takes a known bus to an unscheduled source (`SRC_NEW`). The schedule store holds the polled `SRC1`/`SRC2` window. Each case checks whether the events API refresh saves the bus from `UnscheduledAudio`, which alerts go out, that the call never waits, and how many API calls were made.

| Case | Setup | Result |
|---|---|---|
| events API success with no transition | bus on Primary; the API returns `MAIN SRC_NEW` | `Primary` from `recheck`; the refresh replaces the polled window, so there's no backup; no alerts |
| events API success as backup is a transition | the API returns `BACKUP SRC_NEW` | `Backup` from `recheck`, with no primary (the polled `SRC1` isn't kept); Primary → Backup Error alerts |
| events API success with a correction | bus on Backup; the API returns `MAIN SRC_NEW` | `Primary` from `recheck`; Correction alerts with the first departure and an end time; 2 API calls |
| events API success with a main and backup pair | the API returns `MAIN NEW_PRI` and `BACKUP SRC_NEW` | `Backup` from `recheck` with both sources from the refresh; Primary → Backup Error alerts |
| events API returns only other outputs | the response has no events for the bus | `UnscheduledAudio` from `cache`; the polled window is kept |
| no events API | no client configured | `UnscheduledAudio` from `cache`; the store is unchanged |
| events API error | API returns 500 | `UnscheduledAudio` from `cache`; 1 API call (no lookahead request); the store is unchanged |
| events API returns a different source | API returns `MAIN OTHER_SRC` | `UnscheduledAudio`; the event reports the polled routing it was decided against |
| events API returns an inactive window | API returns `MAIN SRC_NEW` with an expired window | `UnscheduledAudio`; the window isn't stored |
| events API returns untagged and non-apg events | events with no `MAIN`/`BACKUP` tag and other tasks | `UnscheduledAudio`; the store is unchanged |

**`TestRecheckRoute`** tests the recheck on its own:

| Case | Result |
|---|---|
| no events API | accepts unscheduled immediately, keeping the original routing and source |
| API match | 2 calls (now and lookahead); `Primary` from `recheck` with the refreshed routing |
| API failure | 1 call (no lookahead request after the failed now request); accepts unscheduled with the original routing and source |
| events API delay pauses before the request | the stub API only gets the new event halfway through `events_api.delay`; the request is sent after the delay, so the new event is found and the bus is `Primary` from `recheck` |

**`TestRecheckRouteLogs`** captures the `ScheduleRecheck` (hit) and `ScheduleRecheckMiss` debug output and checks the exact log line. Every line includes the destination, the source, the reason or state, and the primary and backup of the active window:

| Case | Expected log line |
|---|---|
| no routing | miss: `Reason:no_active_routing`, blank primary and backup |
| refreshed routing with a different source | miss: `Reason:source_not_scheduled` with the refreshed primary and backup |
| refresh error checks the polled routing | miss: `Reason:source_not_scheduled` with the polled primary and backup |
| hit | hit: `State:Primary` with the refreshed primary and backup |

**`TestIsUnscheduledTransition`** checks when a recheck is triggered: only for a notification that moves a known bus into `UnscheduledAudio`. It isn't triggered for queries, for a bus with no earlier state, or for a bus that was already unscheduled.

**`TestRefreshOutput`** checks that the recheck runs the poll for one bus: two requests, `output=BUS1&start=now&end=now&task=apg` then the same with `now+25m`, and that only the requested bus is replaced:
- `BUS1`'s polled window is replaced by the current window (primary and backup, even though the two events aren't next to each other in the list) and the upcoming window; an event in both responses is stored once.
- events for `BUS2` and `BUS3` in the same response are ignored: `BUS2` keeps its stored window and `BUS3` isn't added.
- a failed lookahead request still applies the current window.
- a failed now request returns an error, skips the lookahead request and keeps the polled window.
- responses with no events for the bus keep the polled window.

**`TestCreateEventFromEdgeConcurrentRecheck`** checks the per-bus lock. Subscription messages are handled concurrently, so a second notification can arrive while the first is rechecking. The stub API takes 150ms per request:

| Case | Result |
|---|---|
| same bus alerts once | two Primary → `SRC_NEW` notifications for `BUS1`, the second sent mid-recheck: one recheck (2 API calls), one set of Primary → UnscheduledAudio alerts, Primary decremented once, bus stored as `UnscheduledAudio` |
| different buses recheck in parallel | `BUS1` and `BUS2` recheck at the same time: both alert, 4 API calls, done in about one recheck's time (not two) |

Without the lock, the first case makes 4 API calls and sends the alerts twice.

## Self-correction (defunct state)

**`TestCreateEventFromEdgeQueryDefunct`** sends queries that disagree with the stored state.

| Case | What it checks |
|---|---|
| backup corrected to primary on the fourth query | **Queries 1–3:** the state stays Backup, the mismatch counter goes from 1 to 3, there are no alerts or end time, and only the current state is counted. **Query 4:** the state is corrected to Primary with the new source; the deviation is cleared; `deviationStartTime` is the seeded transition time and `deviationEndTime` is now; a `previous` Backup / `current` Primary pair of Correction alerts is sent with `eventType: query` and the trigger `GraphQL Query (Defunct Correction)`. **Query 5:** nothing more is sent, and the times go back to `-`. |
| correction to primary reports when the bus first left primary | with an earlier departure recorded, the corrected event's `deviationStartTime` is that departure time |
| primary corrected to backup on the fourth query | **Queries 1–3:** no alerts. **Query 4:** the state is corrected to Backup and a new deviation starts now; Error alerts are sent, the `previous` one starting at `-`; there's no `deviationEndTime`. |
| same state reports the transition time | a matching query on a deviated bus reports its transition time |
| same primary state with no transition | a matching query on Primary reports `-` |
| query does not recheck | an unscheduled query result doesn't wait or call the events API |

## Multi-step sequence

**`TestCreateEventFromEdgeSequence`** follows one bus through a series of notifications and a final query:

1. **Primary → Backup:** Error alerts; the first departure from Primary is recorded.
2. **Backup → UnscheduledAudio** (no events API): Error alerts; the first departure time is kept.
3. **UnscheduledAudio → Primary:** Correction alerts; `deviationStartTime` is the departure from step 1, not the change in step 2; `deviationEndTime` is now.
4. **Query on Primary:** `deviationStartTime: -`, no alerts.

Times are moved back between steps, so the test can tell "first departure" apart from "last change".

## Notifier fallback

**`TestCreateEventFromEdgeNoopNotifier`** runs a Primary → Backup transition with no notifier configured. It checks that there's no failure, that the alerts are still built (with an empty origin host), and that the counters, the bus state and the deviation time are all updated.

## Schedule poll and refresh

**`TestPollSchedule`** runs the schedule poll against a stub events API:

| Case | What it checks |
|---|---|
| builds the store from now and the lookahead | two requests, `start=end=now` then `start=end=now+25m`, both with `task=apg`; `BUS1` gets its folded current window then the upcoming one; a window returned by both requests is stored once; an untagged bus isn't stored; a bus missing from the poll is removed; the event counters are updated |
| a failed now fetch keeps the previous schedule | no lookahead request is sent and the store is unchanged |
| a failed lookahead fetch still loads now | the current windows are stored |
| a refresh during the poll is kept | a bus refreshed while the poll is in flight keeps the refreshed routing; the other buses get the polled routing |

**`TestRelativeTime`** checks that the lookahead is sent as `now+25m`, `now+1h`, `now+45s`, or `now` for zero.

**`TestSaveCaches`** saves the bus cache and the schedule store in a temporary directory and loads them back the way startup does. The bus state is restored, and only the windows that haven't ended come back (expired ones are pruned before saving).

## Schedule package

The `beater/schedule` package holds the event model, the routing windows and the schedule store.

**`TestParseEvents`** checks payload parsing:
- **Accepted and kept:** a single object; an array; leading whitespace; an array that mixes valid and invalid items (the valid ones are kept).
- **Accepted but skipped:** non-`apg` objects (including ones shaped differently); `apg` appearing in some field other than `task`; a `task` that isn't a string; an `apg` event that doesn't fit the event format; JSON scalars (string, number, `null`); an empty object or array; an array of scalars.
- **Rejected as invalid:** an empty body; text that isn't JSON; a malformed array or object; an object with data after it; a malformed item inside an array.

**`TestEventRouting`** checks how one scheduler event becomes a routing window: `MAIN` sets the primary and `BACKUP` sets the backup; an event with neither tag is rejected; a window in the past is still built (the store decides what's active); a bad date is rejected.

**`TestIsActive`** checks that window edges count as active and that a missing start or end is never active.

**`TestMergeBusRouting`** checks how a window combines with a stored one: a new bus gets a copy, overlapping windows merge (a `BACKUP` fills in alongside an existing `MAIN`), and a non-overlapping window replaces it. Neither input is changed.

**`TestBuildRoutes`** groups an ungrouped list of events: `MAIN` and `BACKUP` events with the same window fold into one routing even when they aren't next to each other; back-to-back windows stay separate; each bus's windows are sorted; untagged events and bad dates are counted and skipped. **`TestBuildRoutesRealPair`** does the same with the real `MAIN`/`BACKUP` pair.

**`TestMergeRouteLists`** merges the now and lookahead results: a duplicate window is stored once, nil and dateless routings are dropped, and the inputs aren't changed.

| Store test | What it checks |
|---|---|
| `TestStoreActive` | the window active now is returned; the upcoming window becomes active when the current one ends; an expired window isn't returned unless end dates aren't validated |
| `TestStoreReplace` | a poll replaces every bus, removes buses missing from it, and keeps a bus refreshed after the poll began |
| `TestStoreActiveOverlap` | overlapping windows resolve to the one whose start is closest to now (a shorter event inside a longer one wins, and the longer one resumes after it); the same start resolves to the shorter window; back-to-back windows hand over at the boundary |
| `TestStoreReplaceOutput` | a refresh replaces only that bus's windows (nothing from the old windows is kept); overlapping events stay separate; a new bus is added; a later poll keeps the refresh; a refresh with no current window keeps the stored entry and doesn't create one; the caller's windows are copied and a routing a reader already holds is never changed in place |
| `TestStorePrune` | expired windows and buses left empty are removed, both in memory and after a save and load |
| `TestStoreConcurrentAccess` | polls, refreshes, pruning and lookups at the same time, under the race detector |

## Real lab data replays

These tests use real Magnum Scheduler events and routebeat documents captured from a lab Elasticsearch and stored in `beater/testdata/`. The labels are kept exactly as recorded.

| Fixture | Content |
|---|---|
| `scheduler_events.json` | a `MAIN`/`BACKUP` pair for bus `PMQE04`, six real tag combinations (`EXTERNAL`, `PxP`, ...), non-`apg` events (`silence`, `xmuds`, `route`), and a schedule document |
| `routebeat_events.json` | 25 recorded query events covering every state, labels containing spaces, unmatched buses, and two blank-source events from an older build |
| `notification_sequence.json` | ordered recorded notifications for buses `BSITAL` and `BSTRJA` |

| Test | What it checks |
|---|---|
| `TestRealEventsDecode` (`schedule`) | every real event decodes, with the window as a UTC date string |
| `TestRealEventsRouting` (`schedule`) | each real tag combination gives the right primary or backup, for example `MAIN`+`EXTERNAL`+`PxP` → primary |
| `TestRealEventsMixedPayload` (`schedule`) | a real mixed payload keeps only the `apg` events; `silence`, `xmuds`, `route` and the schedule document are dropped |
| `TestBuildRoutesRealPair` (`schedule`) | the real `MAIN`/`BACKUP` pair folds into one window with the primary and backup |
| `TestRealRoutebeatReplay` | each recorded destination is rebuilt and checked again: the labels, status, schedule primary and backup, and counters must match what was recorded. The two blank-source events recorded as `Backup` by the older build are now expected to be `unknown`. |
| `TestRealNotificationSequence` | replays each bus's recorded notifications in order. Every step must give the recorded status; every change must send the right `previous`/`current` alerts (Error, or Correction on a return to Primary, with `deviationEndTime`); steps with no change must send nothing. |

To refresh the fixtures from a lab (read-only):

```bash
python3 -I beater/testdata/fetch_fixtures.py http://<elasticsearch>:9200
```

## Supporting packages

| Package | Tests | What they check |
|---|---|---|
| `beater/eventsapi` | `TestNewValidation`, `TestRequestURL`, `TestEventsAt`, `TestOutputEventsAt`, `TestEventsErrors` | the URL is required, needs a scheme and host, and can't use `{output}`; the parameter names default to `start`/`end`/`output`/`task`; the poll and refresh requests carry the right parameters (configurable names, `now+25m` sent as `now%2B25m`, the task filter, existing URL parameters kept) and the configured headers; the response is parsed into `apg` events; a non-2xx response, a malformed body or a timeout is an error; the per-output recheck requests always send a start and end and use `timeout`, and the poll uses `bulk_timeout` |
| `beater` (logging) | `TestBuildEventsProcessResultsLogs`, `TestEventDestination` | a notification batch's summary log lists its destinations by mapping label (matched and unmatched, not discarded); a query batch's summary has no destinations; without a label the Magnum destination name is used |
| `beater` (logging) | `TestProcessResultsSelector` | the `ProcessResults.Query` and `ProcessResults.Notifications` sub selectors each enable only their event type; `ProcessResults.*` and `ProcessResults` enable both; `*` logs each under its own sub selector; unrelated selectors log nothing |
| `config` | config tests | defaults and parsing of the `schedule` and `events_api` options (including `events_api.delay`, default `0s`, and an empty `task`), and that old `elasticsearch` and `webhook` blocks don't break parsing |

## Known behaviour pinned by the tests

Some tests lock in current behaviour on purpose, so that any change to it is a visible decision:

- **First deviation start time.** When a bus leaves Primary for the first time (with no earlier restore recorded), the return to Primary reports the **last** change as the deviation start, not the moment it left Primary. See *backup to primary on the first deviation reports the last transition*.
- **New bus on Primary.** The first notification for a bus that's on Primary sends a single `current` Correction alert.
- **Self-correction counters.** A self-correction doesn't move counts between states; the next query cycle rebuilds the counters.
