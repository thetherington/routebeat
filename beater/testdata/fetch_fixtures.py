#!/usr/bin/env python3
"""Snapshot real lab data into the beater test fixtures.

Not run by the tests. Re-run to refresh the fixtures:

    python3 -I beater/testdata/fetch_fixtures.py [http://10.9.0.69:9200]

Writes next to this script:
    scheduler_events.json       scheduler webhook payloads (from log-magnum-scheduler-*)
    routebeat_events.json       routebeat documents for the status replay test
    notification_sequence.json  ordered routebeat notifications per bus

The scheduler index stores start/end as epoch-ns numbers (converted at ingest),
the webhook and events api send them as strings, so they are converted back here.
"""

import json
import os
import sys
import urllib.request

ES = sys.argv[1] if len(sys.argv) > 1 else "http://10.9.0.69:9200"
OUT = os.path.dirname(os.path.abspath(__file__))

SCHEDULER = "log-magnum-scheduler-*"
ROUTEBEAT = "routebeat-8.18.0"

# routebeat documents before this time were produced by an older build
CURRENT_BUILD_FROM = "2026-06-01T00:00:00Z"

SEQUENCE_BUSES = ["BSITAL", "BSTRJA"]

ROUTEBEAT_FIELDS = [
    "@timestamp", "eventType", "dstId", "dstName", "dstTag", "dstTags", "dstType", "dstIsSub", "dstIsDst",
    "dstNameset", "destinationLabel", "sourceLabel", "routeableTerminal", "schedule",
]


def search(index, body):
    req = urllib.request.Request(
        f"{ES}/{index}/_search",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        return json.load(resp)


def hits(index, query, size=1, sort="desc", source=None):
    body = {"size": size, "query": query, "sort": [{"@timestamp": sort}]}
    if source:
        body["_source"] = source
    return [h["_source"] for h in search(index, body)["hits"]["hits"]]


def webhook_payload(event):
    """scheduler.event as the webhook sends it: start/end as strings, no ingest-only fields"""
    event = dict(event)
    for key in ("start", "end"):
        if isinstance(event.get(key), (int, float)):
            event[key] = str(int(event[key]))
    return event


def term(field, value):
    return {"term": {field: value}}


def bool_filter(*filters, must_not=None):
    q = {"bool": {"filter": list(filters)}}
    if must_not:
        q["bool"]["must_not"] = must_not
    return q


def tags_exactly(tags):
    """apg events whose tags are exactly the given set"""
    script = "def t = new ArrayList(doc['scheduler.event.tags']); Collections.sort(t); return String.join('+', t) == params.combo"
    return bool_filter(
        term("scheduler.event.task", "apg"),
        {"script": {"script": {"source": script, "params": {"combo": "+".join(sorted(tags))}}}},
    )


def scheduler_fixture():
    # a MAIN and BACKUP event for the same output and window
    pair = None
    for backup in hits(SCHEDULER, tags_exactly(["BACKUP"]), size=50, source=["scheduler.event"]):
        b = backup["scheduler"]["event"]
        mains = hits(SCHEDULER, bool_filter(
            term("scheduler.event.task", "apg"),
            term("scheduler.event.tags", "MAIN"),
            term("scheduler.event.output", b["output"]),
            term("scheduler.event.start", b["start"]),
        ), source=["scheduler.event"])
        if mains:
            pair = [webhook_payload(mains[0]["scheduler"]["event"]), webhook_payload(b)]
            break

    if pair is None:
        sys.exit("no MAIN/BACKUP pair found")

    combos = []
    for tags in (["EXTERNAL", "MAIN"], ["MAIN", "PxP"], ["BACKUP", "PxP"], ["EXTERNAL", "MAIN", "PxP"],
                 ["BACKUP", "EXTERNAL", "PxP"], ["BACKUP", "EXTERNAL"]):
        found = hits(SCHEDULER, tags_exactly(tags), source=["scheduler.event"])
        if found:
            combos.append(webhook_payload(found[0]["scheduler"]["event"]))

    non_apg = []
    for task in ("silence", "xmuds", "route"):
        found = hits(SCHEDULER, term("scheduler.event.task", task), source=["scheduler.event"])
        if found:
            non_apg.append(webhook_payload(found[0]["scheduler"]["event"]))

    # schedule documents have no task, but nest apg events
    schedule = hits(SCHEDULER, bool_filter(must_not=[{"exists": {"field": "scheduler.event.task"}}]),
                    source=["scheduler.schedule"])[0]["scheduler"]["schedule"]
    for ev in schedule.get("events", []):
        for key in ("start", "end"):
            if isinstance(ev.get(key), (int, float)):
                ev[key] = str(int(ev[key]))
    for key in ("start", "end"):
        if isinstance(schedule.get(key), (int, float)):
            schedule[key] = str(int(schedule[key]))

    return {
        "source": f"{ES}/{SCHEDULER}",
        "apg_pair": pair,
        "apg_tag_combos": combos,
        "non_apg": non_apg,
        "schedule_shaped": schedule,
    }


def latest_per_bus(query, limit):
    body = {
        "size": 0,
        "query": query,
        "aggs": {"bus": {"terms": {"field": "schedule.buscode", "size": limit}, "aggs": {
            "doc": {"top_hits": {"size": 1, "sort": [{"@timestamp": "desc"}], "_source": ROUTEBEAT_FIELDS}}}}},
    }
    buckets = search(ROUTEBEAT, body)["aggregations"]["bus"]["buckets"]
    return [b["doc"]["hits"]["hits"][0]["_source"] for b in buckets]


def routebeat_fixture():
    current = {"range": {"@timestamp": {"gte": CURRENT_BUILD_FROM}}}
    query = term("eventType", "query")
    cases = []

    for status in ("Primary", "Backup", "Zorro", "TDA", "UnscheduledAudio"):
        for doc in latest_per_bus(bool_filter(query, current, term("schedule.status", status)), 5):
            cases.append({"name": f"{status} {doc['schedule']['buscode']}", "expect": status, "doc": doc})

    # source labels with spaces
    for label in ("AES IN 004", "XMPROG TX"):
        found = hits(ROUTEBEAT, bool_filter(query, term("sourceLabel", label)), source=ROUTEBEAT_FIELDS)
        if found:
            doc = found[0]
            expect = doc["schedule"].get("status", "not_found")
            cases.append({"name": f"label with spaces {label!r}", "expect": expect, "doc": doc})

    # older build: blank source matched a blank backup label and reported Backup
    old = hits(ROUTEBEAT, bool_filter(
        query, term("schedule.status", "Backup"),
        must_not=[{"exists": {"field": "routeableTerminal.subscribedSource"}}],
    ), size=2, source=ROUTEBEAT_FIELDS)
    for doc in old:
        cases.append({
            "name": f"blank source recorded as Backup by an older build {doc['schedule']['buscode']}",
            "expect": "unknown",
            "note": "the older build matched a blank source to a blank backup label; a blank source is now reported as unknown",
            "doc": doc,
        })

    # buses with no schedule
    for doc in latest_per_bus(bool_filter(query, current, term("schedule.matched", False)), 3):
        cases.append({"name": f"unmatched {doc.get('destinationLabel')}", "expect": "not_found", "doc": doc})

    return {"source": f"{ES}/{ROUTEBEAT}", "cases": cases}


def notification_fixture():
    sequences = []
    for bus in SEQUENCE_BUSES:
        docs = hits(ROUTEBEAT, bool_filter(term("eventType", "notification"), term("schedule.buscode", bus)),
                    size=100, sort="asc", source=ROUTEBEAT_FIELDS)
        sequences.append({"bus": bus, "docs": docs})
    return {"source": f"{ES}/{ROUTEBEAT}", "sequences": sequences}


def write(name, data):
    path = os.path.join(OUT, name)
    with open(path, "w") as f:
        json.dump(data, f, indent=2, sort_keys=True)
        f.write("\n")
    print("wrote", path)


if __name__ == "__main__":
    write("scheduler_events.json", scheduler_fixture())
    write("routebeat_events.json", routebeat_fixture())
    write("notification_sequence.json", notification_fixture())
