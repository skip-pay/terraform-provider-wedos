---
page_title: "WEDOS API behaviour"
subcategory: ""
description: |-
  How WEDOS DNS publishes changes and which API rules the provider enforces.
---

# WEDOS API behaviour

WEDOS DNS differs from DNS services with transactional APIs. This page lists what the provider
does about it and what remains visible to you. Values marked *observed* were measured against
the live API in October 2026 and may change, as WEDOS describes the REST API as still in
development.

## Writes are staged and committed

Record changes are staged in WEDOS and published by a commit. The provider commits the zone after
every resource that changed something (`commit = true`, the default). With `commit = false`, WEDOS
publishes staged changes automatically about five minutes after the last change.

There is no transaction: a failed apply leaves the changes that were already written, and WEDOS
publishes them. The provider orders writes so that a name never answers with nothing in between:
values are added or rewritten before others are deleted. The exception is a change between CNAME
and another type at the same name, where the old records must go first.

## Changes reach the name servers in a five-minute cycle

*Observed:* WEDOS publishes committed changes every five minutes (at minutes ending in 0 and 5).
The four WEDOS name servers then receive the change within another 1–2.5 minutes. From commit to
all name servers took 1.2–7.3 minutes; resolvers may keep the previous answer for up to the
record's TTL after that. WEDOS documents propagation as usually within 30–60 minutes.

`wait_for_reload = true` waits until the WEDOS API reports the commit processed, typically 7–10
seconds. It does not wait for the name servers, which the API does not expose.

## Record rules enforced at plan time

| Rule | WEDOS response otherwise |
| --- | --- |
| TTL between 300 and 172800 seconds (*observed*) | error `2317` |
| `ALIAS` only at the zone apex (`name = ""`) | error `2312` |
| `CAA` value quoted: `0 issue "letsencrypt.org"` | error `2314` |
| One value per `CNAME` record set | error `2313` |
| `author_comment` with ASCII letters, digits and spaces only | error `2298` |
| Record names lowercase and relative to the zone, `""` for the apex | names are stored lowercased |

## Values WEDOS rewrites

WEDOS stores host names in record values without the trailing dot and lowercases CNAME and MX
targets. The provider compares values the way WEDOS stores them and keeps your spelling in state,
so either form works in configuration. Write host names without the trailing dot: after an import
there is no configuration to copy the spelling from. `ALIAS` targets must not end with a dot;
the provider removes it before sending.

TXT values longer than 255 characters (DKIM keys) are sent as one string; WEDOS splits them into
255-character strings when serving them.

## ALIAS records

An `ALIAS` at the apex answers with the addresses of its target, resolved by WEDOS. It cannot
coexist with apex `A` or `AAAA` records. WEDOS synchronises the addresses without a guaranteed
interval, and resolves the target from its own location, so a CDN target answers with the edge
nearest to WEDOS for every client.

## Request budget and concurrency

An API client may make 1,000 requests per hour, rejected requests included. The provider lists
each zone once per Terraform run and serves all record reads from that list, does not retry
writes, and serialises writes per zone within one run. WEDOS has no concurrency control between
separate runs: do not apply two configurations to the same zone at the same time.

## Apex NS records

WEDOS manages the apex NS records of a zone. `wedos_dns_records_exclusive` ignores them and
rejects them in configuration.
