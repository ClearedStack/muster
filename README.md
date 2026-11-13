<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Muster" height="56">
  </picture>
</p>

<p align="center">
  <strong>Hands out work to the workers, records every result and change of state, and receives edit proposals.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: first build.** Muster runs alone and passes its tests against an in-memory stand-in for Logbook. It has not been connected to a real Logbook and has not been run in a cluster. [What works today](#what-works-today) and [Not yet done](#not-yet-done) say exactly where it stands.

Muster is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Muster is meant to be understood and used without the rest.

## In this repository

| Path | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Muster does, its interfaces, how it fails and how it is kept safe. The section **Contract v1** was added during the first build. |
| [`PROMPT.md`](PROMPT.md) | The prompt the build session was started with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`cmd/muster`](cmd/muster) | The program. |
| [`internal/`](internal) | Its parts, one package each. See [Layout](#layout). |
| [`examples/seed.json`](examples/seed.json) | Eight made-up items to start the stand-in with when trying Muster by hand. |
| [`.ko.yaml`](.ko.yaml) | How `ko` builds the image from source. There is no Dockerfile. |
| [`brand/`](brand) | The Muster logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `hub-queue-api` |
| **Namespace** | `clearedstack` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/muster` |
| **Memory budget** | 48 MiB |
| **Depends on** | [Logbook](https://github.com/ClearedStack/logbook) |
| **Depended on by** | [Conn](https://github.com/ClearedStack/clearedstack.com), [Shipkeeper](https://github.com/ClearedStack/shipkeeper), [Trawl](https://github.com/ClearedStack/trawl), [Sextant](https://github.com/ClearedStack/sextant), [Assay](https://github.com/ClearedStack/assay), [Countersign](https://github.com/ClearedStack/countersign), [Waybill](https://github.com/ClearedStack/waybill), [Moldloft](https://github.com/ClearedStack/moldloft), [Shakedown](https://github.com/ClearedStack/shakedown), [Chartroom](https://github.com/ClearedStack/chartroom), [Tidegauge](https://github.com/ClearedStack/tidegauge), [Bellbuoy](https://github.com/ClearedStack/bellbuoy) |

### Boundaries

- Worker calls over cluster-private HTTP: lease, heartbeat, complete, fail, needs_subtool
- Reads and writes Logbook's working records; the only service that does
- Live update stream and edit-proposal intake for Conn
- Receives health reports from Shipkeeper

## What works today

Everything below is built, and each rule has saved examples that fail if the behaviour changes.

- **The five worker calls.** A worker leases its next step, keeps the lease with heartbeats, and ends it with complete, fail or needs_subtool. Work is offered urgent first, then normal, then backfill, and in order of arrival within a lane.
- **The fourteen item states.** Only the moves in the specification's table are allowed, and each worker can make only its own. A refused move changes nothing.
- **State and queue together.** A change of state and the next queue step are written in one transaction, or neither is.
- **Leases that expire.** A step whose lease runs out is offered again. A finished result that arrives late is still kept if nobody else has taken the step.
- **Repeats.** A worker that repeats a call it was never told had succeeded gets the same answer, and nothing happens twice.
- **Pause and replay.** An item with no rule is paused. When Shipkeeper reports a new image digest for the worker that paused it, the item runs again in the urgent lane.
- **Health reports.** A report is a closed form with no free-text field, so it cannot carry a node name or an address.
- **The live feed.** Queue depths, component health and edit status changes, as Server-Sent Events, in one batch every five seconds.
- **Edit intake, checks 1 to 4.** Proof of work, honeypot, a daily-salted rate limit that never stores an address, and the shape and size check with markup stripped. What passes is kept as `pending_approval` and queued for Countersign. Check 5, the owner's approval, happens there: Countersign leases the proposal and completes it as approved or declined.
- **Identity.** Every caller presents its Kubernetes ServiceAccount token, and each may make only its own calls.
- **Three error codes.** Every refusal is one of a fixed list of 22 sentences with one of the three codes. None is built from request data.
- **Small and unprivileged.** One static binary that writes nothing to disk. Under the load test it peaks at about 38 MiB of the 48 MiB budget, most of it the stand-in’s copy of the records; idle it holds about 7 MiB.

## Not yet done

- **A real Logbook.** Muster talks to Logbook through a small interface ([`internal/logbook`](internal/logbook/logbook.go)). The only thing behind it today is a stand-in that keeps everything in memory and loses it when the process stops. The SQL adapter, and with it the append-only ledger kept in full, are not built, because Logbook's tables do not exist yet. The stand-in keeps only the most recent 2,048 ledger lines.
- **Creating items.** None of the five calls records a new candidate. The stand-in can be started with a file of made-up items; nothing else can add one.
- **Tidegauge's work.** Its moves are in the state table, but no queue step is defined for a scheduled check, so Tidegauge is always told there is nothing to lease.
- **Bellbuoy's routing of failures and paused items.** Only its step for approved edits is built.
- **A proposal's last step,** from `building` to `live`. Nothing says who reports it.
- **Releasing an item stopped by `E_PARSE`.** It is held and never offered again.
- **The identity check against a real cluster.** It is tested against saved answers from a stand-in for the Kubernetes API, not against a cluster.
- **Three duties named on other pages and not in `SPEC.md`:** rejecting raw Sextant pages bound for Moldloft, requiring a written reason for an override, and routing the owner's install, upgrade, pause and rollback actions.
- **Kubernetes manifests.** None is included. The form Shipkeeper expects is not specified.
- **The image has not been pushed or signed.** `ko build` was run without pushing and produced a 3.5 MiB image whose user is not root.

`SPEC.md` lists the open questions behind most of these.

## Run it

You need Go 1.24 or later. Muster has no other dependencies.

```sh
go build -o muster ./cmd/muster

MUSTER_AUTH=file \
MUSTER_AUTH_FILE=internal/httpapi/testdata/callers.json \
MUSTER_OWNERSHIP_MAP=internal/httpapi/testdata/ownership_map.json \
MUSTER_SALT_FILE=internal/httpapi/testdata/salt.txt \
MUSTER_SEED_FILE=examples/seed.json \
./muster
```

The files under `testdata` hold made-up tokens, a made-up ownership map and a made-up salt. They are for trying Muster on your own machine and for the tests, and for nothing else.

In another terminal, be Sextant: take the next item and report it observed.

```sh
curl -s -X POST localhost:8080/v1/lease \
  -H 'Authorization: Bearer made-up-token-for-sextant' \
  -H 'Content-Type: application/json' \
  -d '{"contract":"v1"}'
# {"lease":"PA-0003.1","kind":"item","id":"PA-0003","state":"recorded","lane":"urgent",...}

curl -s -X POST localhost:8080/v1/complete \
  -H 'Authorization: Bearer made-up-token-for-sextant' \
  -H 'Content-Type: application/json' \
  -d '{"contract":"v1","lease":"PA-0003.1","state":"observed","result":{"job":"made-up job"}}'
# {"id":"PA-0003","state":"observed","next":"assay"}
```

Be Conn and watch the feed:

```sh
curl -N localhost:8080/v1/feed -H 'Authorization: Bearer made-up-token-for-conn'
```

### Settings

All settings come from the environment. One that is missing where it is required, or that does not parse, stops Muster from starting.

| Variable | Default | Meaning |
| --- | --- | --- |
| `MUSTER_AUTH` | required | `kubernetes` to ask the cluster whose token a caller presents, or `file` to use a fixed table of made-up tokens |
| `MUSTER_AUTH_FILE` | | The table, when `MUSTER_AUTH` is `file` |
| `MUSTER_OWNERSHIP_MAP` | required | Path of `ownership_map.json` |
| `MUSTER_SALT_FILE` | required | Path of the secret the daily salt is made from; at least 32 bytes |
| `MUSTER_SEED_FILE` | | Made-up items to start the stand-in with |
| `MUSTER_LISTEN` | `:8080` | Address to listen on |
| `MUSTER_LEASE_SECONDS` | `60` | How long a lease protects a step |
| `MUSTER_FEED_SECONDS` | `5` | How often the feed sends a batch; at most 10 |
| `MUSTER_STORE_SECONDS` | `5` | How long a call may wait for Logbook before answering `E_TIMEOUT` |
| `MUSTER_POW_BITS` | `20` | Zero bits the proof of work must show |
| `MUSTER_EDIT_DAILY_CAP` | `5` | Edit submissions kept per source per day |

### Build the image

```sh
ko build ./cmd/muster
```

## Tests

```sh
go test ./...          # everything, about a minute
go test -short ./...   # skips the tests that build and load the real binary
```

The saved examples are the record of how Muster behaves:

| Where | What is saved |
| --- | --- |
| [`internal/httpapi/testdata/cases/`](internal/httpapi/testdata/cases) | 23 short stories, one per rule. Each is a list of calls with the exact status and body expected back. Start here to see what a call does. |
| [`internal/lifecycle/testdata/tables.json`](internal/lifecycle/testdata/tables.json) | Every state, move, lane and queue step. A second test checks these against the tables in `SPEC.md`. |
| [`internal/contract/testdata/refusals.json`](internal/contract/testdata/refusals.json) | Every refusal Muster can give. |
| [`internal/intake/testdata/`](internal/intake/testdata) | Markup stripping, proof-of-work tokens and source-address hashes. The tokens and hashes were computed outside Muster's own code. |
| [`internal/feed/testdata/stream.txt`](internal/feed/testdata/stream.txt) | The exact bytes a feed subscriber is sent. |
| [`internal/queue/testdata/ledger.jsonl`](internal/queue/testdata/ledger.jsonl) | The ledger lines one short story writes. |
| [`internal/auth/testdata/`](internal/auth/testdata) | The identity question Muster sends the cluster, and saved answers. |

To change a rule on purpose, change the code, run the package's tests with `-update`, and read the difference in the saved files. Every changed line is a change in behaviour.

All data in the tests is made up: the items, the tokens, the proposals and the addresses, which come from the ranges reserved for documentation.

## Layout

| Package | What it holds |
| --- | --- |
| `internal/registry` | The names Muster may use, from the naming registry |
| `internal/contract` | The contract version, the three codes and the fixed list of refusals |
| `internal/lifecycle` | The tables: states, moves, lanes and queue steps |
| `internal/logbook` | The interface Logbook sits behind, and `memory`, the stand-in |
| `internal/queue` | Lease, heartbeat, complete, fail, needs_subtool and replay |
| `internal/health` | Shipkeeper's reports |
| `internal/intake` | The edit checks: puzzle, honeypot, rate limit, stripping, ownership map |
| `internal/feed` | The live feed |
| `internal/auth` | Who is calling |
| `internal/httpapi` | The HTTP surface |
| `internal/config` | Settings from the environment |

## Names

`muster` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
