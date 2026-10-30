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
- [Scheduler events and the hot cache](#scheduler-events-and-the-hot-cache)
- [Real lab data replays](#real-lab-data-replays)
- [Supporting packages](#supporting-packages)
- [Known behaviour pinned by the tests](#known-behaviour-pinned-by-the-tests)

---

## How the tests are built

The tests call the deviation engine directly with Magnum SDVN destinations built in memory, so they need no Magnum, Elasticsearch or notification server.

| Building block | Purpose |
|---|---|
| **Test destination** | a destination `BUS1` on tag `MES`, with a Broadview nameset. Options control its tags, its subscribed source label and source tags, and its routed physical source. |
| **Seeded schedule** | an active schedule for `BUS1` with primary `SRC1` and backup `SRC2`, as if it came from the Elasticsearch poll |
| **Seeded bus state** | a stored state and source for the bus, with deviation times set minutes in the past so each time can be told apart from "now" |
| **Stub notifier** | captures alerts instead of sending them |
| **Stub events API** | a local HTTP server that returns a chosen scheduler payload or error status and counts how many calls it received |
| **Webhook delivery** | posts a scheduler payload into routebeat's webhook handler partway through the unscheduled wait |
| **Short wait** | `webhook.wait` is 50ms in tests, so waiting and not waiting can be measured reliably |

Each test starts from empty caches. Times are compared exactly when they're seeded, and as "within 2 seconds of now" when routebeat sets them.

Each case can check any of the following:
- the `schedule.*` fields (status, routing source, primary, backup, matched);
- every alert: whether it's `previous` or `current`, Error or Correction, its status, source, start and end;
- the counter changes for the tag;
- the stored bus state and source afterwards;
- whether the unscheduled recheck waited, and how many events API calls it made.

## Coverage map

| Area | Tests |
|---|---|
| Event fields, namesets, labels | `TestCreateEventFromEdgeBuild` |
| Regular cache vs hot cache | `TestCreateEventFromEdgeScheduleLookup`, `TestGetBusRoutingFromCaches` |
| Primary / Backup / Zorro / TDA / Unscheduled | `TestCreateEventFromEdgeStateResolution`, `TestMatchScheduledState`, `TestResolveRoutingState` |
| Unknown (blank source) | `TestCreateEventFromEdgeBlankSource` |
| Transitions, alerts, counters, deviation times | `TestCreateEventFromEdgeTransitions`, `TestCreateEventFromEdgeNotificationNoChange`, `TestCreateEventFromEdgeSequence` |
| Unscheduled recheck | `TestCreateEventFromEdgeUnscheduledRecheck`, `TestRecheckHotRoute`, `TestIsUnscheduledTransition` |
| Self-correction | `TestCreateEventFromEdgeQueryDefunct` |
| Scheduler payloads and hot cache | `TestParseSchedulerEvents`, `TestSchedulerWebhookResponses`, `TestSchedulerAPGEventBusRouting`, `TestMergeBusRouting`, `TestApplySchedulerEvents`, `TestPruneScheduleHotCache`, `TestEventsAPIRefreshUpdatesHotCache`, `TestEventsAPIRefreshUpdatesMultipleOutputs` |
| Real lab data | `TestRealSchedulerEvents*`, `TestRealRoutebeatReplay`, `TestRealNotificationSequence` |

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
| expired ES entry with end date validation | not found |
| expired ES entry without end date validation | found, `routingSource: cache` (development setting) |
| active ES entry | `routingSource: cache` with the ES primary and backup |
| active hot entry with webhook enabled | `routingSource: hotcache` with the hot primary and backup, overriding ES |
| active hot entry ignored when webhook and events API are off | `routingSource: cache`; the hot cache isn't used |
| expired hot entry falls back to ES | `routingSource: cache` |

**`TestGetBusRoutingFromCaches`** tests the same rules on the lookup alone: cache only, hot cache wins, hot cache disabled, hot cache only, not found, and expired cache entry.

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
| no recheck with the hot cache enabled | no wait, no events API call |

This replaces an older behaviour in which a blank source matched a blank schedule backup and was reported as `Backup`. That behaviour was found in real lab data.

## Notifications with no state change

**`TestCreateEventFromEdgeNotificationNoChange`**

| Case | Expected |
|---|---|
| same state updates the source label | Primary → Primary on a new source: the stored source is updated, `deviationStartTime: -`, no alerts |
| same state reports the existing transition time | Backup → Backup: `deviationStartTime` is the seeded transition time |
| unscheduled to unscheduled does not recheck | no wait and no events API call, even with the hot cache enabled |

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
| primary to unscheduled with the hot cache disabled | `previous` Primary + `current` UnscheduledAudio (Error) | Primary −1, Unscheduled +1 | no recheck wait |
| new bus to unscheduled does not recheck | `current` UnscheduledAudio | Unscheduled +1 | no wait, no events API call |

## Unscheduled recheck

**`TestCreateEventFromEdgeUnscheduledRecheck`** sends a notification that takes a known bus to an unscheduled source (`SRC_NEW`) with the hot cache enabled. Each case checks whether the recheck saves the bus from `UnscheduledAudio`, and which alerts go out.

| Case | Setup | Result |
|---|---|---|
| webhook success with no transition | bus on Primary; the webhook posts `MAIN SRC_NEW` during the wait | `Primary` from `hotcache`; no alerts |
| webhook success as backup is a transition | the webhook posts `BACKUP SRC_NEW` | `Backup` from `hotcache`; Primary → Backup Error alerts |
| events API success with a correction | bus on Backup; the API returns `MAIN SRC_NEW` | `Primary` from `hotcache`; Correction alerts; no wait; 1 API call |
| events API success returns before the webhook wait | API match with the webhook also on | returns without waiting |
| webhook only failure | nothing arrives | `UnscheduledAudio` from `cache`; waited the full time; Error alerts |
| events API error without webhook | API returns 500 | `UnscheduledAudio`; no wait; 1 API call |
| events API error falls back to the webhook | API returns 500, then the webhook posts `MAIN SRC_NEW` | `Primary` from `hotcache` |
| events API returns a different source | API returns `MAIN OTHER_SRC` | `UnscheduledAudio`; falls through to the webhook wait |
| events API returns an inactive window | API returns `MAIN SRC_NEW` with an expired window | `UnscheduledAudio`; the event isn't cached |
| events API returns untagged and non-apg events | events with no `MAIN`/`BACKUP` tag and other tasks | `UnscheduledAudio`; the hot cache stays empty |
| hot entry present for a different source | hot cache already holds `MAIN OTHER_SRC` | `UnscheduledAudio`; the schedule shown is the hot one |
| expired hot entry for the source | hot cache holds `MAIN SRC_NEW` with a past window | `UnscheduledAudio` from `cache` |

**`TestRecheckHotRoute`** tests the recheck on its own:

| Case | Result |
|---|---|
| webhook only waits | waits the full time |
| API match returns without waiting | 1 call, no wait |
| API only does not wait | 1 call, no wait |
| API failure falls back to the webhook wait | 1 call, then waits |
| API failure without webhook does not wait | accepts unscheduled immediately |
| webhook data arriving during the wait is used | a payload posted halfway through the wait is picked up |

**`TestIsUnscheduledTransition`** checks when a recheck is triggered: only for a notification that moves a known bus into `UnscheduledAudio`. It isn't triggered for queries, for a bus with no earlier state, or for a bus that was already unscheduled.

**`TestEventsAPIRefreshUpdatesHotCache`** checks that an events API response with `MAIN`, `BACKUP` and unrelated events fills in the hot cache's primary and backup.

**`TestEventsAPIRefreshUpdatesMultipleOutputs`** checks that a response for one bus can carry events for other buses, and that all of them are merged into the hot cache. The request is for `BUS1`:
- `BUS1` gets its primary and backup, even though the two events aren't next to each other in the list.
- `BUS2` gets a new primary and keeps the backup it already had, because the windows overlap.
- `BUS3` gets a backup only.

## Self-correction (defunct state)

**`TestCreateEventFromEdgeQueryDefunct`** sends queries that disagree with the stored state.

| Case | What it checks |
|---|---|
| backup corrected to primary on the fourth query | **Queries 1–3:** the state stays Backup, the mismatch counter goes from 1 to 3, there are no alerts or end time, and only the current state is counted. **Query 4:** the state is corrected to Primary with the new source; the deviation is cleared; `deviationStartTime` is the seeded transition time and `deviationEndTime` is now; a `previous` Backup / `current` Primary pair of Correction alerts is sent with `eventType: query` and the trigger `GraphQL Query (Defunct Correction)`. **Query 5:** nothing more is sent, and the times go back to `-`. |
| correction to primary reports when the bus first left primary | with an earlier departure recorded, the corrected event's `deviationStartTime` is that departure time |
| primary corrected to backup on the fourth query | **Queries 1–3:** no alerts. **Query 4:** the state is corrected to Backup and a new deviation starts now; Error alerts are sent, the `previous` one starting at `-`; there's no `deviationEndTime`. |
| same state reports the transition time | a matching query on a deviated bus reports its transition time |
| same primary state with no transition | a matching query on Primary reports `-` |
| query does not recheck the hot cache | an unscheduled query result doesn't wait or call the events API |

## Multi-step sequence

**`TestCreateEventFromEdgeSequence`** follows one bus through a series of notifications and a final query:

1. **Primary → Backup:** Error alerts; the first departure from Primary is recorded.
2. **Backup → UnscheduledAudio** (hot cache off): Error alerts; the first departure time is kept.
3. **UnscheduledAudio → Primary:** Correction alerts; `deviationStartTime` is the departure from step 1, not the change in step 2; `deviationEndTime` is now.
4. **Query on Primary:** `deviationStartTime: -`, no alerts.

Times are moved back between steps, so the test can tell "first departure" apart from "last change".

## Notifier fallback

**`TestCreateEventFromEdgeNoopNotifier`** runs a Primary → Backup transition with no notifier configured. It checks that there's no failure, that the alerts are still built (with an empty origin host), and that the counters, the bus state and the deviation time are all updated.

## Scheduler events and the hot cache

These tests cover the payloads that reach the hot cache from the webhook and the events API.

**`TestParseSchedulerEvents`** checks payload parsing:
- **Accepted and kept:** a single object; an array; leading whitespace; an array that mixes valid and invalid items (the valid ones are kept).
- **Accepted but skipped:** non-`apg` objects (including ones shaped differently); `apg` appearing in some field other than `task`; a `task` that isn't a string; an `apg` event that doesn't fit the event format; JSON scalars (string, number, `null`); an empty object or array; an array of scalars.
- **Rejected as invalid:** an empty body; text that isn't JSON; a malformed array or object; an object with data after it; a malformed item inside an array.

**`TestSchedulerWebhookResponses`** sends 14 payloads end to end through the real webhook handler. Only an empty body, whitespace, non-JSON text or malformed JSON gets **400**. Inactive events, untagged events, events with bad dates, events that don't fit the format, and non-`apg` events all get **202** and are skipped silently.

**`TestSchedulerAPGEventBusRouting`** checks how one scheduler event becomes a routing:
- `MAIN` sets the primary and `BACKUP` sets the backup;
- an event with neither tag is rejected;
- events before or after their window are rejected, and the window edges count as active;
- a bad date is rejected.

**`TestMergeBusRouting`** checks how an event combines with what's already stored: a new bus gets a copy, overlapping windows merge (a `BACKUP` event fills in alongside an existing `MAIN`), and a non-overlapping window replaces the entry.

**`TestApplySchedulerEvents`** checks how a batch of events updates the hot cache:
- a `MAIN`/`BACKUP` pair fills in one bus;
- untagged and inactive events aren't cached, and they're counted as such;
- a later `MAIN`-only update replaces the primary but keeps the backup;
- an entry a reader already holds is never changed in place.

**`TestSchedulerHookCallbackConcurrentReads`** writes to the hot cache and reads it at the same time, under the race detector, to prove the hot cache is safe to share.

**`TestPruneScheduleHotCache`** checks that expired, empty and dateless entries are removed, both in memory and after the hot cache is reloaded from file. Active entries are kept.

## Real lab data replays

These tests use real Magnum Scheduler events and routebeat documents captured from a lab Elasticsearch and stored in `beater/testdata/`. The labels are kept exactly as recorded.

| Fixture | Content |
|---|---|
| `scheduler_events.json` | a `MAIN`/`BACKUP` pair for bus `PMQE04`, six real tag combinations (`EXTERNAL`, `PxP`, ...), non-`apg` events (`silence`, `xmuds`, `route`), and a schedule document |
| `routebeat_events.json` | 25 recorded query events covering every state, labels containing spaces, unmatched buses, and two blank-source events from an older build |
| `notification_sequence.json` | ordered recorded notifications for buses `BSITAL` and `BSTRJA` |

| Test | What it checks |
|---|---|
| `TestRealSchedulerEventsDecode` | every real event decodes, with the window as a UTC date string |
| `TestRealSchedulerEventsBusRouting` | each real tag combination gives the right primary or backup, for example `MAIN`+`EXTERNAL`+`PxP` → primary |
| `TestRealSchedulerEventsMixedPayload` | a real mixed payload keeps only the `apg` events; `silence`, `xmuds`, `route` and the schedule document are dropped |
| `TestRealSchedulerEventsPairUpdatesHotCache` | the real `MAIN`/`BACKUP` pair fills the hot cache whether it's sent in one payload, in two separate payloads (merged), or as an events API response |
| `TestRealRoutebeatReplay` | each recorded destination is rebuilt and checked again: the labels, status, schedule primary and backup, and counters must match what was recorded. The two blank-source events recorded as `Backup` by the older build are now expected to be `unknown`. |
| `TestRealNotificationSequence` | replays each bus's recorded notifications in order. Every step must give the recorded status; every change must send the right `previous`/`current` alerts (Error, or Correction on a return to Primary, with `deviationEndTime`); steps with no change must send nothing. |

To refresh the fixtures from a lab (read-only):

```bash
python3 -I beater/testdata/fetch_fixtures.py http://<elasticsearch>:9200
```

## Supporting packages

| Package | Tests | What they check |
|---|---|---|
| `beater/eventsapi` | `TestNewValidation`, `TestRequestURL`, `TestRefresh` | the URL is required; `{output}` is replaced in the URL, or `?output=` is added; the response body is passed on for processing; errors from processing it are returned; a non-2xx response or a timeout is an error and nothing is processed |
| `beater/httpserver` | `TestSchedulerEventRouting`, `TestHandleSchedulerEvent`, `TestStart*`, `TestWriteJSON` | only `POST /api/v1/scheduler/events` is routed; 202 / 400 / 405 / 500 responses; the payload reaches processing unchanged and with the request context; the server starts and shuts down cleanly; a port already in use is reported |
| `config` | config tests | defaults and parsing of the `webhook` and `events_api` options |

## Known behaviour pinned by the tests

Some tests lock in current behaviour on purpose, so that any change to it is a visible decision:

- **First deviation start time.** When a bus leaves Primary for the first time (with no earlier restore recorded), the return to Primary reports the **last** change as the deviation start, not the moment it left Primary. See *backup to primary on the first deviation reports the last transition*.
- **New bus on Primary.** The first notification for a bus that's on Primary sends a single `current` Correction alert.
- **Self-correction counters.** A self-correction doesn't move counts between states; the next query cycle rebuilds the counters.
