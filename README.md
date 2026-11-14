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

> **Status: ready to deploy.** Muster runs alone on an in-memory stand-in for Logbook, and statelessly against Logbook's schema (migrations 0001 to 0010) in PostgreSQL 17. Its tests pass on both, and hardened Kubernetes manifests live under [`deploy/`](deploy), which Argo CD applies to run it in a cluster. [What works today](#what-works-today) and [Not yet done](#not-yet-done) say exactly where it stands.

Muster is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Muster is meant to be understood and used without the rest.

## In this repository

| Path | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Muster does, its interfaces, how it fails and how it is kept safe. |
| [`contract/v1/CONTRACT.md`](contract/v1/CONTRACT.md) | The contract: every call, state, queue step and refusal, exactly. Build a worker against this. |
| [`contract/v1/examples/`](contract/v1/examples) | One saved example for each call and each refusal, replayed by a test. |
| [`PROMPT.md`](PROMPT.md) | The prompt the build session was started with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`cmd/muster`](cmd/muster) | The program. |
| [`internal/`](internal) | Its parts, one package each. See [Layout](#layout). |
| [`deploy/`](deploy) | The kustomize directory Argo CD applies: ServiceAccount and its `system:auth-delegator` binding, ConfigMap, Deployment, Service, NetworkPolicy and the `cs-workers` namespace. See [`deploy/README.md`](deploy/README.md). |
| [`kustomization.yaml`](kustomization.yaml) | Points at `deploy/`, so `kubectl kustomize .` works from the top. |
| [`examples/seed.json`](examples/seed.json) | Eight made-up items to start the stand-in with when trying Muster by hand. |
| [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) | The licence notices of pgx and the three modules it brings. |
| [`.ko.yaml`](.ko.yaml) | How `ko` builds the image from source. There is no Dockerfile. |
| [`brand/`](brand) | The Muster logo and mark, for light and dark backgrounds. |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `hub-queue-api` |
| **Namespace** | `clearedstack` |
| **Container image** | `muster`, with the registry and digest given by a kustomize image override when deployed |
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
- **The fourteen item states.** Only the moves in the contract's table are allowed, and each worker can make only its own. A refused move changes nothing.
- **State and queue together.** A change of state and the next queue step are written in one transaction, or neither is.
- **Results passed on.** Each worker's result reaches every later worker under that worker's name, for items and proposals alike.
- **Leases that expire.** A step whose lease runs out is offered again. A finished result that arrives late is still kept if nobody else has taken the step.
- **Repeats.** A worker that repeats a call it was never told had succeeded gets the same answer, and nothing happens twice.
- **Recording candidates.** Trawl or Conn records a find as an item, once. An address already on record changes nothing and the answer names the item that has it.
- **Pause and replay.** An item with no rule is paused. When Shipkeeper reports a new image digest for the worker that paused it, the item runs again in the urgent lane.
- **Incidents.** An item that is stopped or paused gets one step for Bellbuoy saying which worker, which code and which state.
- **Release.** The owner's decision, passed on by Countersign, returns a stopped item to the urgent lane in the state it was stopped in.
- **Checks on live items.** Every live item comes to Tidegauge every seven days, in the backfill lane. A check can leave it live, send it back to be rebuilt, or retire it.
- **Health reports.** A report is a closed form with no free-text field, so it cannot carry a node name or an address.
- **The live feed.** Queue depths, component health and edit status changes, as Server-Sent Events, in one batch every five seconds.
- **Edit intake, checks 1 to 4.** Proof of work, honeypot, a daily-salted rate limit that never stores an address, and the shape and size check with markup stripped. What passes is kept as `pending_approval` and queued for Countersign. Check 5, the owner's approval, happens there. An approved proposal goes live when Shipkeeper reports a new digest for its repository.
- **Identity.** Every caller presents its Kubernetes ServiceAccount token, and each may make only its own calls.
- **Refusals.** Every refusal is a status, one of the three codes, a cause a program can act on and a reason for a person, all from a fixed list of 28. None is built from request data.
- **Logbook.** With `MUSTER_DATABASE_URL` set, Muster keeps every durable record in Logbook's own tables (migrations 0001 to 0010) through the `muster` role: items and their intake sources, the queue and held steps, the ledger, pauses, edit proposals with reasons and file paths, worker results, lease outcomes, evaluations, decisions, component health and image digests, and daily visitor submission counts. Muster holds nothing beside Logbook in memory, so a restart loses nothing. Logbook gives out every identifier, a change of state, its ledger row and the next queue step are one PostgreSQL transaction, the 52 contract examples and 30 multi-step cases pass against it with the same expected bytes as against the stand-in, and Logbook's 19-check `core.integrity_check()` finds nothing wrong with what Muster writes.
- **Small and unprivileged.** One static binary that writes nothing to disk. Under the load test it peaks at about 38 MiB of the 48 MiB budget, most of it the stand-in’s copy of the records; idle it holds about 7 MiB.

## Not yet done

- **Three of Tidegauge's moves.** `parked`, `linked` or `merged` back to `observed` are in the state table and have no next worker defined, so they are refused.
- **The identity check against a real cluster.** It is tested against saved answers from a stand-in for the Kubernetes API, not against a cluster.
- **Three duties named on other pages and not in `SPEC.md` or the contract:** rejecting raw Sextant pages bound for Moldloft, requiring a written reason for an override in the in-memory stand-in (Logbook's `core.check_decision` already requires one), and routing the owner's install, upgrade, pause and rollback actions.
- **The image is not built or signed by Keelworks yet.** [`deploy/muster.yaml`](deploy/muster.yaml) names the image `muster` only; whoever deploys Muster builds it with `ko` and gives the registry and digest as an image override.

`SPEC.md` lists the open questions behind these, and the choices the contract left to the build.

## Run it

You need Go 1.24 or later. Muster has one dependency, [pgx](https://github.com/jackc/pgx), which it uses to reach Logbook; see [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

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
# {"lease":"PA-0003.3.1","kind":"item","id":"PA-0003","state":"recorded","lane":"urgent",...}

curl -s -X POST localhost:8080/v1/complete \
  -H 'Authorization: Bearer made-up-token-for-sextant' \
  -H 'Content-Type: application/json' \
  -d '{"contract":"v1","lease":"PA-0003.3.1","state":"observed","result":{"job":"made-up job"}}'
# {"id":"PA-0003","state":"observed","next":"assay"}
```

Be Trawl and record a new find:

```sh
curl -s -X POST localhost:8080/v1/items \
  -H 'Authorization: Bearer made-up-token-for-trawl' \
  -H 'Content-Type: application/json' \
  -d '{"contract":"v1","source":"sam-gov","title":"A made-up notice","address":"https://example.org/made-up/notice-1"}'
# {"id":"PA-0009","state":"discovered","next":"trawl"}
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
| `MUSTER_DATABASE_URL` | | Where Logbook is, as a PostgreSQL address for the `muster` role. Without it Muster uses the in-memory stand-in |
| `MUSTER_DATABASE_PASSWORD` | | Password for the `muster` role when kept in a separate Secret instead of `MUSTER_DATABASE_URL` |
| `MUSTER_SEED_FILE` | | Made-up items to start the stand-in with. Not allowed with `MUSTER_DATABASE_URL` |
| `MUSTER_LISTEN` | `:8080` | Address to listen on |
| `MUSTER_LEASE_SECONDS` | `60` | How long a lease protects a step |
| `MUSTER_FEED_SECONDS` | `5` | How often the feed sends a batch; at most 10 |
| `MUSTER_STORE_SECONDS` | `5` | How long a call may wait for Logbook before answering `E_TIMEOUT` |
| `MUSTER_POW_BITS` | `20` | Zero bits the proof of work must show |
| `MUSTER_EDIT_DAILY_CAP` | `5` | Submissions kept per visitor per day, edit proposals and site-form candidates together |

### Build the image and manifests

```sh
ko build ./cmd/muster
kubectl kustomize .
```

The manifests under [`deploy/`](deploy) expect two Secrets in namespace `clearedstack`, neither kept in Git: `muster-database` (`url`, the PostgreSQL address of Logbook for the `muster` role) and `muster-salt` (`salt`, at least 32 random bytes). [`deploy/README.md`](deploy/README.md) lists everything a deployer supplies.

## Tests

```sh
go test ./...          # everything, about a minute
go test -short ./...   # skips the tests that build and load the real binary
```

The tests against Logbook need a PostgreSQL server and a checkout of Logbook, and are skipped without them:

```sh
MUSTER_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
MUSTER_LOGBOOK_MIGRATIONS=/path/to/logbook/migrations \
go test ./...
```

Each test makes a database of its own with Logbook's migrations applied, writes to it as the `muster` role, and removes it afterwards.

The saved examples are the record of how Muster behaves:

| Where | What is saved |
| --- | --- |
| [`contract/v1/examples/`](contract/v1/examples) | One file for each call and each refusal in the contract: the request, without the token, and the exact status and body expected. The test fails if a call or a refusal has no example. |
| [`internal/httpapi/testdata/cases/`](internal/httpapi/testdata/cases) | 30 short stories, one per rule. Each is a list of calls in order with the exact status and body expected back. Start here to see how the calls work together. |
| [`internal/lifecycle/testdata/tables.json`](internal/lifecycle/testdata/tables.json) | Every state, move, lane and queue step. A second test checks these against the tables in the contract. |
| [`internal/contract/testdata/refusals.json`](internal/contract/testdata/refusals.json) | Every refusal Muster can give. |
| [`internal/intake/testdata/`](internal/intake/testdata) | Markup stripping, proof-of-work tokens and address hashes. The tokens and hashes were computed outside Muster's own code. |
| [`internal/feed/testdata/stream.txt`](internal/feed/testdata/stream.txt) | The exact bytes a feed subscriber is sent. |
| [`internal/queue/testdata/ledger.jsonl`](internal/queue/testdata/ledger.jsonl) | The ledger lines one short story writes. |
| [`internal/auth/testdata/`](internal/auth/testdata) | The identity question Muster sends the cluster, and saved answers. |

To change a rule on purpose, change the code, run the package's tests with `-update`, and read the difference in the saved files. Every changed line is a change in behaviour.

All data in the tests is made up: the items, the tokens, the proposals and the addresses, which come from the ranges reserved for documentation.

## End-to-end harness

[`e2e/`](e2e) is the acceptance test of Phase 1. One invented item, the Wrap Rate Calculator, goes from intake to `live` through the real worker binaries, against this Muster and a real local PostgreSQL 17 with Logbook's migrations 0001 to 0010. It runs only with the build tag `e2e`, so `go test ./...` is unchanged. It uses the standard library and the pgx this repository already has, and it execs the workers; it never imports their code.

You need Go 1.24 or later, `git`, and PostgreSQL 17 on `PATH` (`initdb`, `pg_ctl`; on macOS, `brew install postgresql@17` and put `$(brew --prefix postgresql@17)/bin` first on `PATH`). Reading the worker repositories needs the Plainforge port-forward on 127.0.0.1:13000 (`kubectl -n clearedstack port-forward svc/plainforge-http 13000:3000`); nothing else in the run leaves the machine.

```sh
sh e2e/build-workers.sh              # clones and builds what workers.txt names, and Muster from this checkout
go test -tags e2e -count=1 -v ./e2e  # runs the stages and prints the stage report
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `E2E_BIN_DIR` | `e2e/bin` | Where `build-workers.sh` puts the binaries (and Logbook's migrations) and where the test finds them. Use an absolute path if you set it, and the same value for both commands. |
| `E2E_LOGBOOK_MIGRATIONS` | `$E2E_BIN_DIR/logbook/migrations` | Logbook's migrations directory. |
| `E2E_PLAINFORGE_BASE` | `http://127.0.0.1:13000/ClearedStack` | Where `build-workers.sh` clones from. |
| `E2E_SHOW_LOGS` | none | Set to anything to print each worker's last output after its stage, as well as when a stage fails. |
| `E2E_STAND_IN` | none | `sextant`: the harness does Sextant's step by hand so that the later workers can be seen running. A run with this set always fails at the end: it is a diagnostic, not an acceptance pass. |

[`e2e/workers.txt`](e2e/workers.txt) names one repository and branch for each worker, and for Logbook's migrations. The run, in order:

1. **infrastructure.** A throwaway PostgreSQL cluster in a temporary directory on a free port, Logbook's migrations applied with the helper the SQL tests use (`internal/logbook/sql/sqltest`), Muster started with `MUSTER_AUTH=file` and one invented token for each caller, a local Git smart HTTP server (`git http-backend` behind Basic auth) with the hub repository `ClearedStack/clearedstack.com.git` and the empty `ClearedStack/wrap-rate-calculator.git`, and a local server for the invented page in [`e2e/testdata/wrap-rate/page.html`](e2e/testdata/wrap-rate/page.html), which carries a canary string.
2. **trawl.** Trawl cannot be given a local source (its fetcher refuses loopback and private addresses), so the harness records the find with `POST /v1/items` as the `trawl` caller, and the real Trawl leases and records it.
3. **sextant, assay.** The real workers.
4. **countersign.** The real Countersign leases the scored item. The owner's decision is made by the harness, and only here: it runs `countersign decide <id> approve -waybill FILE` with the invented specification in [`e2e/testdata/wrap-rate-waybill.json`](e2e/testdata/wrap-rate-waybill.json) (made-up worked examples on the public idea of a wrap rate, FAR 31.203, no real rates).
5. **waybill, moldloft, shakedown, chartroom.** The real workers. Moldloft pushes the generated repository and Chartroom writes the catalog branch over the local Git server.
6. **outcome.** The item is `live`; `refs/heads/catalog/wrap-rate-calculator` holds the entry in `catalog/tools.json` and `main` in the hub is the commit it started at; the generated repository exists and the canary appears nowhere in it (files, names, commits); `pub.item_history` holds every change of state, by the right worker, in order; `core.integrity_check()` finds nothing wrong.

The test prints a stage report. The first stage that fails stops the run and says why, with the item's state, queue, ledger and results and the worker's last output; the stages after it are reported as not reached. A worker defect or a mismatch with the contract is a finding for the owner of that worker: the harness reports it and does not hide it (a run that stands in for a worker never passes). Every process it starts is killed when the test ends (or on Ctrl-C), every port is chosen at run time, and the tokens and the Git credentials are invented fresh for each run and never printed.

## Layout

| Package | What it holds |
| --- | --- |
| `internal/registry` | The names Muster may use, from the naming registry |
| `internal/contract` | The contract version, the three codes and the fixed list of refusals |
| `internal/lifecycle` | The tables: states, moves, lanes and queue steps |
| `internal/logbook` | The interface Logbook sits behind; `sql`, Logbook in PostgreSQL; and `memory`, the stand-in |
| `internal/queue` | Lease, heartbeat, complete, fail, needs_subtool, replay, incidents and release |
| `internal/health` | Shipkeeper's reports |
| `internal/intake` | The edit checks (puzzle, honeypot, rate limit, stripping, ownership map) and the recording of candidates |
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
