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

> **Status: design.** The first milestone is built: the five worker calls over HTTP against an in-memory store. Everything else in the specification is not built yet. See [What works today](#what-works-today).

Muster is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Muster is meant to be understood and used without the rest.

## What works today

One small Go program, standard library only, that serves the five worker calls against an **in-memory stand-in for Logbook**. It keeps nothing after a restart. It is for trying the calls and for the tests; it is not ready to run in the cluster.

| Call | What it does |
| --- | --- |
| `POST /v1/lease` | Hands the worker the next item waiting in the state it names: `urgent` before `normal` before `backfill`, and within a lane the one that arrived first. `204` with no body when nothing is waiting. |
| `POST /v1/heartbeat` | Extends a lease the worker still holds. |
| `POST /v1/complete` | Records the result and moves the item to the state the worker names, if the table allows that move. The move and the item's next place in the queue are recorded together. |
| `POST /v1/fail` | Records a failure. After `E_TIMEOUT` the item goes back in the queue. After `E_PARSE` work on it stops. The item keeps its state. |
| `POST /v1/needs_subtool` | Pauses the item in `needs_subtool`, remembering the state it was paused in and the worker that paused it. |

Also working:

- **The fourteen item states and the 25 moves allowed between them**, in [`internal/state`](internal/state/state.go). Any other move is refused and changes nothing. The table comes from the published page [An item's life](https://clearedstack.com/platform/item-lifecycle/), because `SPEC.md` does not list the states.
- **Leases that run out.** An item whose lease has run out is offered again in its old place in the queue. Once it has been handed to someone else, the first holder's reports are refused.
- **Reports delivered twice.** A worker that did not hear the answer can send the same report again and gets the same answer. It is recorded once.
- **An append-only ledger** of every lease, expiry, result, failure and pause, in the stand-in.
- **Three public error codes and no others.** A refused request gets `E_PARSE` and a fixed sentence. A record of state that takes too long gets `E_TIMEOUT`. No error names a machine, an address or a credential.

### Not yet done

Everything else in `SPEC.md`:

- A real connection to Logbook (PostgreSQL). Only the in-memory stand-in exists.
- Checking a worker's Kubernetes ServiceAccount. Today a worker is whoever it says it is, as long as the name is one of the ten in the registry.
- Replaying paused items when Shipkeeper deploys a new worker version. The move out of `needs_subtool` is in the table, and nothing triggers it yet.
- The live update stream (SSE) for Conn.
- Edit-proposal intake and its five anti-spam layers.
- Health reports from Shipkeeper.
- The `ko` image build, the deployment manifests, and a measured check against the 48 MiB budget in the cluster.
- A way for new items to enter. Today items come only from a file of made-up items loaded at start.

### Run it

Needs Go 1.24 or later. Nothing else.

```sh
go test ./...                                   # the tests, including the saved examples
MUSTER_SEED=examples/seed.json go run ./cmd/muster
```

Then, in another terminal:

```sh
curl -X POST localhost:8080/v1/lease -d '{"worker":"sextant","state":"recorded"}'
curl -X POST localhost:8080/v1/complete -d '{"worker":"sextant","item":"PA-0002","lease_id":"L-00000001","to":"observed","result":{"job":"made-up example"}}'
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `MUSTER_ADDR` | `:8080` | Address to listen on. |
| `MUSTER_LEASE_SECONDS` | `60` | How long a lease lasts without a heartbeat. |
| `MUSTER_SEED` | none | A JSON file of made-up items to load at start. See [`examples/seed.json`](examples/seed.json). |

### Saved examples

Every rule has a saved example: a file in [`internal/api/testdata/cases`](internal/api/testdata/cases) holding made-up items, the calls made in order, and the exact answers, ledger lines and records expected. The tests replay each file and compare byte for byte. [`internal/state/testdata/transitions.json`](internal/state/testdata/transitions.json) is the saved copy of the state table. After a deliberate change of behaviour, `go test ./internal/api -run TestSavedExamples -update` rewrites the expected output, and the diff shows what changed.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Muster does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Muster logo and mark, for light and dark backgrounds. |
| [`cmd/muster`](cmd/muster) | The program. |
| [`internal/state`](internal/state) | The fourteen states and the moves allowed between them. |
| [`internal/logbook`](internal/logbook) | The small interface Muster uses to reach Logbook, and the in-memory stand-in. |
| [`internal/queue`](internal/queue) | The rules for the five worker calls. |
| [`internal/api`](internal/api) | The five calls over HTTP, and the saved examples. |

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

## Names

`muster` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
