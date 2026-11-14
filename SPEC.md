# Muster specification

> Central queue lease manager, state-transition gatekeeper, SSE live feed publisher, and 5-layer anti-spam edit intake in the clearedstack namespace.

**Status: design, with a first build.** This is a specification. It describes how Muster is meant to work. A first build exists in this repository; the [README](README.md) says what it does today and what it does not.

The same text is published at [clearedstack.com/platform/muster/](https://clearedstack.com/platform/muster/). To change the specification, change it here and say why.

Muster (`v0.1.0-draft`, in the `muster` repository) is the sole read-write gatekeeper in front of Logbook. It coordinates all 10 deterministic workers in `cs-workers`, streams live status to Conn over Server-Sent Events (SSE), and protects anonymous edit intake with a 5-layer anti-spam shield.

## Overview & Role

Rather than letting 10 worker pods hold database passwords or implement their own locking, Muster centralizes all state transitions and queue coordination:

- **Sole `core.*` Writer:** Besides Logbook itself, Muster is the only service in the cluster with read/write access to `core.*` tables.
- **Lease & Heartbeat Coordinator:** Issues work leases in priority order (`urgent` -> `normal` -> `backfill`), tracks worker heartbeats, and commits state changes alongside the next queue step in a single Postgres transaction.
- **`needs_subtool` Pause & Replay:** Places items encountering unknown layouts into `needs_subtool` and automatically replays them once Shipkeeper deploys the updated worker digest.
- **SSE Live Feed:** Publishes real-time queue depths, worker health, and edit status transitions to Conn.
- **5-Layer Anti-Spam Edit Intake:** Screens anonymous `✎ Edit` submissions before they enter Countersign as `pending_approval`.

## Runtime & Resource Budget

| Attribute | Value |
| --- | --- |
| **Namespace** | `clearedstack` (Control Plane) |
| **Kind** | `control-plane` |
| **Version** | `v0.1.0-draft` |
| **Owning Repo** | `muster` |
| **RAM Request** | `48 MiB` |
| **Priority Class** | High (Control Plane - protected from eviction) |
| **Storage** | Stateless (all durable state lives in Logbook) |
| **Network Exposure** | Cluster-private (`ClusterIP` only); reachable by Conn, Shipkeeper, and `cs-workers` |

## Interfaces & Data Contracts

| Peer Service | Direction | Protocol / Auth | Data Contract |
| --- | --- | --- | --- |
| **Logbook** | Outbound | SQL (read/write on `core.*`) | Atomic transactions for item state, queue leases, edit proposals, and append-only ledger |
| **`cs-workers` (Trawl through Bellbuoy)** | Inbound | Cluster-private HTTP (Kubernetes ServiceAccount) | Lease acquisition (`urgent`/`normal`/`backfill`), heartbeats, result submission, and `E_NEEDS_SUBTOOL` reporting |
| **Conn** | Outbound / Inbound | Cluster-private HTTP + SSE | Streams live SSE progress; receives anonymous `✎ Edit` proposals |
| **Shipkeeper** | Inbound | Cluster-private HTTP | Receives sanitized pod health and stripped error events (zero node names or internal IPs) |

### 5-Layer Anti-Spam Edit Intake

Every anonymous `✎ Edit` submission must pass all five layers before reaching Countersign:

1. **Browser Proof-of-Work:** Validates the ~1-second client-side cryptographic puzzle token.
2. **Honeypot Field:** Silently discards requests where the hidden form field is populated.
3. **Daily-Salted IP Rate Limit:** Hashes the client IP with a daily-rotating secret salt and caps submissions per hash without ever storing raw IP addresses.
4. **Strict Schema Check:** Verifies the target exists in `ownership_map.json`, enforces a 4 KB payload limit, and strips all HTML and scripts.
5. **Review Gate:** Holds every valid proposal in `pending_approval` inside Countersign until explicitly approved by the human owner.

## Failure Modes & Recovery

- **Hub Restart During Worker Execution:** Because Muster is a single control-plane pod, a rolling upgrade or restart makes it briefly unreachable. Workers stage completed outputs in their **local spool volume** first, then retry delivery to Muster on a short backoff until acknowledged, deleting the spooled file only after Postgres confirms receipt.
- **Worker Crash Mid-Lease:** If a worker pod dies before acknowledging a task, its lease expires in Logbook and Muster re-leases the unfinished item to a healthy worker.
- **SSE Disconnect or Stale Feed:** If the SSE stream to Conn stalls for more than 15 seconds, deterministic attention rules surface a P1 warning on the Command Portal.
- **High-Load Contention:** High Priority Class keeps Muster scheduled ahead of workers and ephemeral Jobs when node memory tightens.

## Security & Clean-Room Boundaries

- **Zero DB Credentials in Workers:** Workers in `cs-workers` authenticate via Kubernetes ServiceAccount HTTP; they never hold Postgres passwords and have no knowledge of `core.*` table layouts.
- **No Public Ingress:** Blocked from direct internet access by default-deny `NetworkPolicy`; only Conn forwards sanitized intake requests.
- **No Automatic Cluster Writes from Public Edits:** Anonymous proposals cannot touch Plainforge or running pods; they remain isolated in `pending_approval` until owner approval.
- **Unprivileged Execution:** Runs as non-root with dropped Linux capabilities and no privilege escalation.

## Contract v1

The calls, the states, the queue and the refusals are defined in [`contract/v1/CONTRACT.md`](contract/v1/CONTRACT.md), with one saved example for each call and each refusal in [`contract/v1/examples/`](contract/v1/examples). The sections above name these things; the contract defines them. Where the two differ in detail, the contract is what the build follows, and the change log below says why.

## Open questions

These could not be settled from the published pages or the contract. The build does not guess at them; the README lists each as not yet done.

1. **Three of Tidegauge's moves.** Every live item comes to Tidegauge for a check every seven days, and a check can leave it live, send it back to be rebuilt or retire it. Three moves in the state table still have no next worker: `parked`, `linked` or `merged` back to `observed`. Nothing says how Tidegauge comes to hold such an item, or which worker follows.
2. **Which worker pauses a `recorded` item.** The lifecycle table says intake has no rule for the source, which points at Trawl, but Trawl holds items in `discovered`, which has no move to `needs_subtool`. The build follows the state table: Sextant can pause a `recorded` item and Trawl cannot pause anything.
3. **Health states.** Shipkeeper's page says it sends `healthy`, `idle`, `degraded` and `paused`. The operations page lists six display states: `healthy`, `idle`, `erroring`, `recovering`, `stale` and `empty`. The build accepts and republishes the four Shipkeeper sends, unchanged, and leaves the mapping to Conn.
4. **Duties named on other pages only.** The pipeline page says the hub rejects any message to Moldloft that carries a raw page captured by Sextant, without saying how one is recognised. Countersign's page says Muster rejects an override with no written reason. Shipkeeper's page says the owner's install, upgrade, pause and rollback actions are routed through Muster. None of these is in this specification or the contract, and none is built.
5. **The hub repository.** `ownership_map.json` is said to live in "the hub repository". This specification assumes that is not Muster's concern: Muster is given the ownership map as a file.
6. **A `DATA` edit.** A proposal goes live when Shipkeeper reports a new digest for its repository. A `DATA` edit changes a record and may never cause a new image, so it would wait in `building` for the next unrelated deploy of that repository.

Choices the contract left to the build, each easy to change and each pinned by a saved example:

- **The cause `duplicate`** is used in one place: a batch of health reports that names the same component twice.
- **A slow identity check** answers `503` with the cause `busy`, since `store_slow` would name the wrong thing.
- **Release** is its own call, `POST /v1/release`, made by Countersign, because a stopped item is never offered for lease.
- **A rebuild** follows the first build: after Tidegauge sends a live item back to `building` for Waybill, Moldloft and then Shakedown each have a step that leaves it in `building`.
- **A lease** is named by its step, the step's place in the order of arrival and the attempt, so that it can be checked against Logbook's own record of the queue.
- **Limits on a title and an address** are counted in bytes, as Logbook counts them.
- **A target in the ownership map** has the form Logbook requires of an element name.
- **A new item's lane** follows its source: `web-sweep` starts in `backfill`, the other three in `normal`.
- **A `site-form` candidate** carries two more fields than the others, `pow` and `visitor`, because the proof of work and the daily limit need them. It shares one daily count with that visitor's edit proposals.
- **An incident step** is in the `normal` lane, and its `state` is the state the item was in when the worker stopped.
- **A live item with no check recorded,** as in a seed file, is due for one at once.
- **The 4 KB limit on an edit proposal** is checked with the other shape checks, after the rate limit, now that every body is read up to 64 KiB.

## How Logbook keeps Muster's records

With Logbook's migrations 0001 to 0010 applied, all durable state Muster needs lives in PostgreSQL under `core.*`, and Muster holds nothing beside Logbook in process memory:

| What Muster keeps | Where Logbook stores it |
| --- | --- |
| Items, their state, title, address and intake source | `core.item` (`source` on candidates) |
| Queue steps, leases, heartbeats and stopped steps | `core.queue` (`held = true` when stopped by `E_PARSE`; future `queued_at` for seven-day checks, excluded from `pub.queue_depth` until due) |
| Append-only transition history | `core.ledger` |
| Paused items waiting on a worker rule | `core.pause` |
| Edit proposals, their element, owning repo, file, reason and status | `core.edit_proposal` (`reason`, `file_path`) |
| Each worker's latest JSON object result on an item or proposal | `core.result` (alongside structured rows in `core.evaluation` and `core.decision`) |
| The answer to the call that ended a subject's last lease | `core.lease_outcome` (items, incidents and proposals) |
| Each component's latest health, error code and image digest | `core.component_health` (published without digests via `pub.component_health`) |
| Each visitor key's daily submission count | `core.submission_count` |

One small mismatch remains and needs no schema change:

- **The pattern of a pause.** `core.pause` requires `pattern`, while `POST /v1/needs_subtool` carries none, so Muster writes `"not given"`.

## Changes to this specification

| Change | Reason |
| --- | --- |
| Added **Contract v1**: states and moves, lanes, queue steps, the five calls, errors, health reports, the live feed and edit intake | The specification named these and did not define them. A worker cannot be written against a name, and a rule with no definition cannot have a saved example |
| Followed the lifecycle page's table over its figure | They disagree, and the table is the more detailed of the two |
| Mapped every refusal to an HTTP status plus the nearest of the three codes | Only three public codes are allowed, and an illegal move, an expired lease and a missing credential fit none of them exactly |
| Required a ServiceAccount token from Conn and Shipkeeper as well as from workers | The interface table lists authentication for workers only, which would let any pod that can reach Muster forge a health report or a proposal |
| Added `GET /healthz` | A pod needs something the cluster can check |
| Moved the contract to `contract/v1/CONTRACT.md`, with saved examples beside it | So that the workers, Conn and Shipkeeper have one file to build against, and examples that are replayed against Muster |
| Spelled the state `screened_out` | One spelling for every state: lower case with underscores |
| Fixed the form of item and proposal identifiers | So that an identifier has exactly one spelling |
| Gave every refusal a `cause` | The three codes cannot tell a caller what to do next; the cause can |
| Added `POST /v1/items` | Nothing could record a new candidate |
| Raised Waybill's `complete` to 256 KiB and set every other body to 64 KiB | A specification is larger than any other result |
| Added the seven-day Tidegauge check and its three outcomes, Bellbuoy's incident step, the move of a proposal to `live` and the owner's release | These were open questions; each now has a rule |
| Named pgx as the one dependency, and added a store that keeps Muster's records in Logbook's schema | Muster is the only writer of Logbook's working records, and the standard library has no PostgreSQL driver |
| Let `complete` take a result for a proposal as it does for an item | One rule for every step a worker completes, and Bellbuoy needs what Countersign decided |
| Stored results, intake sources, proposal reasons and file paths, stopped steps, lease outcomes, component health and daily visitor counts in Logbook migration 0010 | Removes the in-memory side store so Muster is stateless across restarts |

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
