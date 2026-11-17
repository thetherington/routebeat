#!/usr/bin/env python3
"""Snapshot real lab data into the beater test fixtures.

Not run by the tests. Re-run to refresh the fixtures:

    # both fixtures sets
    python3 -I beater/testdata/fetch_fixtures.py \\
        --events-api https://<host>:20000/api/v1/scheduler/events --insecure \\
        --es http://<elasticsearch>:9200

    # only the scheduler fixture, from the events api or from a saved response
    python3 -I beater/testdata/fetch_fixtures.py scheduler --events-api https://<host>:20000/api/v1/scheduler/events --insecure
    python3 -I beater/testdata/fetch_fixtures.py scheduler --events-file tools/mock_events_api/data.json

    # only the routebeat fixtures, from elasticsearch
    python3 -I beater/testdata/fetch_fixtures.py routebeat --es http://<elasticsearch>:9200

Writes next to this script:
    scheduler_events.json       scheduler events as the events api returns them
    routebeat_events.json       routebeat documents for the status replay test (elasticsearch)
    notification_sequence.json  ordered routebeat notifications per bus (elasticsearch)

The scheduler fixture is picked from the events active now (start=now&end=now):
    apg_pair        a MAIN and BACKUP event for the same output and window
    apg_tag_combos  one event per MAIN/BACKUP tag combined with EXTERNAL or PxP
    deleted         apg events in the deleted state (7), which routebeat ignores
    non_apg         events from a request without the task filter whose task isn't apg
                    (empty when building from a saved task=apg response)
"""

import argparse
import json
import os
import ssl
import sys
import urllib.parse
import urllib.request

OUT = os.path.dirname(os.path.abspath(__file__))

STATE_DELETED = 7

# extra tag sets to capture, alongside the plain MAIN/BACKUP pair
TAG_COMBOS = (["EXTERNAL", "MAIN"], ["MAIN", "PxP"], ["BACKUP", "PxP"], ["EXTERNAL", "MAIN", "PxP"],
              ["BACKUP", "EXTERNAL", "PxP"], ["BACKUP", "EXTERNAL"])

DELETED_LIMIT = 3
NON_APG_LIMIT = 3

ROUTEBEAT = "routebeat-8.18.0"

# routebeat documents before this time were produced by an older build
CURRENT_BUILD_FROM = "2026-06-01T00:00:00Z"

SEQUENCE_BUSES = ["BSITAL", "BSTRJA"]

ROUTEBEAT_FIELDS = [
    "@timestamp", "eventType", "dstId", "dstName", "dstTag", "dstTags", "dstType", "dstIsSub", "dstIsDst",
    "dstNameset", "destinationLabel", "sourceLabel", "routeableTerminal", "schedule",
]


def get_json(url, ctx=None, data=None):
    headers = {"Accept": "application/json"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=120, context=ctx) as resp:
        return json.load(resp)


# ---------- scheduler fixture (events api) ----------

def events_url(base, **params):
    u = urllib.parse.urlsplit(base)
    q = urllib.parse.parse_qsl(u.query) + list(params.items())
    return urllib.parse.urlunsplit(u._replace(query=urllib.parse.urlencode(q)))


def as_list(payload):
    return payload if isinstance(payload, list) else [payload]


def is_apg(e):
    return isinstance(e, dict) and e.get("task") == "apg"


def tag_set(e):
    return sorted(e.get("tags") or [])


def stable(events):
    """deterministic order so a re-run on the same data writes the same fixture"""
    return sorted(events, key=lambda e: (e.get("output", ""), str(e.get("start", "")), e.get("event_id", "")))


def scheduler_fixture(apg_events, all_events, source):
    live = stable(e for e in apg_events if is_apg(e) and e.get("state") != STATE_DELETED)

    # a MAIN and BACKUP event for the same output and window
    mains = {(e["output"], e["start"], e["end"]): e for e in live if tag_set(e) == ["MAIN"]}
    pair = next(([mains[k], e] for e in live if tag_set(e) == ["BACKUP"]
                 for k in [(e["output"], e["start"], e["end"])] if k in mains), None)
    if pair is None:
        sys.exit("no MAIN/BACKUP pair found")

    combos = []
    for tags in TAG_COMBOS:
        found = next((e for e in live if tag_set(e) == sorted(tags)), None)
        if found:
            combos.append(found)
        else:
            print(f"no event tagged {'+'.join(tags)}", file=sys.stderr)

    deleted = stable(e for e in apg_events if is_apg(e) and e.get("state") == STATE_DELETED)[:DELETED_LIMIT]
    if not deleted:
        print("no deleted (state 7) events", file=sys.stderr)

    # one event per non-apg task
    non_apg, seen = [], set()
    for e in stable(e for e in all_events if isinstance(e, dict) and not is_apg(e)):
        if e.get("task") not in seen and len(non_apg) < NON_APG_LIMIT:
            seen.add(e.get("task"))
            non_apg.append(e)

    return {
        "source": source,
        "apg_pair": pair,
        "apg_tag_combos": combos,
        "deleted": deleted,
        "non_apg": non_apg,
    }


def scheduler_from_api(base, ctx):
    apg_url = events_url(base, task="apg", start="now", end="now")
    all_url = events_url(base, start="now", end="now")
    print("GET", apg_url)
    apg_events = as_list(get_json(apg_url, ctx))
    print("GET", all_url)
    all_events = as_list(get_json(all_url, ctx))
    return scheduler_fixture(apg_events, all_events, apg_url)


def scheduler_from_file(path):
    with open(path) as f:
        events = as_list(json.load(f))
    return scheduler_fixture(events, events, os.path.basename(path))


# ---------- routebeat fixtures (elasticsearch) ----------

class ES:
    def __init__(self, url):
        self.url = url.rstrip("/")

    def search(self, index, body):
        return get_json(f"{self.url}/{index}/_search", data=json.dumps(body).encode())

    def hits(self, index, query, size=1, sort="desc", source=None):
        body = {"size": size, "query": query, "sort": [{"@timestamp": sort}]}
        if source:
            body["_source"] = source
        return [h["_source"] for h in self.search(index, body)["hits"]["hits"]]

    def latest_per_bus(self, query, limit):
        body = {
            "size": 0,
            "query": query,
            "aggs": {"bus": {"terms": {"field": "schedule.buscode", "size": limit}, "aggs": {
                "doc": {"top_hits": {"size": 1, "sort": [{"@timestamp": "desc"}], "_source": ROUTEBEAT_FIELDS}}}}},
        }
        buckets = self.search(ROUTEBEAT, body)["aggregations"]["bus"]["buckets"]
        return [b["doc"]["hits"]["hits"][0]["_source"] for b in buckets]


def term(field, value):
    return {"term": {field: value}}


def bool_filter(*filters, must_not=None):
    q = {"bool": {"filter": list(filters)}}
    if must_not:
        q["bool"]["must_not"] = must_not
    return q


def routebeat_fixture(es):
    current = {"range": {"@timestamp": {"gte": CURRENT_BUILD_FROM}}}
    query = term("eventType", "query")
    cases = []

    for status in ("Primary", "Backup", "Zorro", "TDA", "UnscheduledAudio"):
        for doc in es.latest_per_bus(bool_filter(query, current, term("schedule.status", status)), 5):
            cases.append({"name": f"{status} {doc['schedule']['buscode']}", "expect": status, "doc": doc})

    # source labels with spaces
    for label in ("AES IN 004", "XMPROG TX"):
        found = es.hits(ROUTEBEAT, bool_filter(query, term("sourceLabel", label)), source=ROUTEBEAT_FIELDS)
        if found:
            doc = found[0]
            expect = doc["schedule"].get("status", "not_found")
            cases.append({"name": f"label with spaces {label!r}", "expect": expect, "doc": doc})

    # older build: blank source matched a blank backup label and reported Backup
    old = es.hits(ROUTEBEAT, bool_filter(
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
    for doc in es.latest_per_bus(bool_filter(query, current, term("schedule.matched", False)), 3):
        cases.append({"name": f"unmatched {doc.get('destinationLabel')}", "expect": "not_found", "doc": doc})

    return {"source": f"{es.url}/{ROUTEBEAT}", "cases": cases}


def notification_fixture(es):
    sequences = []
    for bus in SEQUENCE_BUSES:
        docs = es.hits(ROUTEBEAT, bool_filter(term("eventType", "notification"), term("schedule.buscode", bus)),
                       size=100, sort="asc", source=ROUTEBEAT_FIELDS)
        sequences.append({"bus": bus, "docs": docs})
    return {"source": f"{es.url}/{ROUTEBEAT}", "sequences": sequences}


def write(name, data):
    path = os.path.join(OUT, name)
    with open(path, "w") as f:
        json.dump(data, f, indent=2, sort_keys=True)
        f.write("\n")
    print("wrote", path)


def main():
    p = argparse.ArgumentParser(description="snapshot lab data into the beater test fixtures")
    p.add_argument("targets", nargs="*", choices=["scheduler", "routebeat"], default=["scheduler", "routebeat"],
                   help="fixtures to write (default: both)")
    p.add_argument("--events-api", help="events api url, e.g. https://<host>:20000/api/v1/scheduler/events")
    p.add_argument("--events-file", help="build the scheduler fixture from a saved events api response instead")
    p.add_argument("--insecure", action="store_true", help="skip tls certificate verification for the events api")
    p.add_argument("--es", help="elasticsearch url for the routebeat fixtures, e.g. http://<elasticsearch>:9200")
    args = p.parse_args()

    if "scheduler" in args.targets:
        if args.events_file:
            write("scheduler_events.json", scheduler_from_file(args.events_file))
        elif args.events_api:
            ctx = ssl._create_unverified_context() if args.insecure else None
            write("scheduler_events.json", scheduler_from_api(args.events_api, ctx))
        else:
            p.error("the scheduler fixture needs --events-api or --events-file")

    if "routebeat" in args.targets:
        if not args.es:
            p.error("the routebeat fixtures need --es")
        es = ES(args.es)
        write("routebeat_events.json", routebeat_fixture(es))
        write("notification_sequence.json", notification_fixture(es))


if __name__ == "__main__":
    main()
