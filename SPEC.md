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
| **Priority Class** | High (Control Plane — protected from eviction) |
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

> **Added during the first build, and not yet published.** The sections above name the calls, the states and the checks without defining them. Nothing can be built or tested against a name, so this section writes down what the first build does. Each part says where it came from. The [change log](#changes-to-this-specification) gives the reason for each addition, and [Open questions](#open-questions) lists what is still undecided.

Every call is JSON over cluster-private HTTP. Every request body carries `"contract": "v1"`. A body with a field the call does not define is refused.

### Item states and moves

An item has an identifier of the form `PA-0001` and exactly one of fourteen states. The states and moves are the table "Which worker makes each move" on the [item lifecycle page](https://clearedstack.com/platform/item-lifecycle/). Where that page's figure and its table disagree, the table is followed.

| From | May move to |
| --- | --- |
| `discovered` | `recorded`, `rejected` |
| `recorded` | `observed`, `needs_subtool` |
| `observed` | `scored`, `screened-out`, `needs_subtool` |
| `screened-out` | `observed` |
| `scored` | `approved`, `parked`, `linked`, `merged`, `rejected` |
| `approved` | `building`, `needs_subtool` |
| `building` | `live`, `parked` |
| `live` | `building`, `retired` |
| `parked` | `observed` |
| `linked` | `observed` |
| `merged` | `observed` |
| `needs_subtool` | the state it was paused from, and no other |
| `rejected` | nowhere |
| `retired` | nowhere |

Any other move is refused and changes nothing.

### Lanes

Every queue step is in one of three lanes. A worker is offered the `urgent` lane first, then `normal`, then `backfill`, and within a lane the step that arrived first.

### Queue steps

The queue holds, for each item or proposal, at most one step: the next thing to do, and which worker does it. Completing a step changes the state and writes the next step in one transaction. Which worker follows is fixed by this table. Each row comes from the published description of the worker named in it.

| Worker | Holds an item in | Completes it to | Next step |
| --- | --- | --- | --- |
| Trawl | `discovered` | `recorded` | Sextant |
| Trawl | `discovered` | `rejected` | none |
| Sextant | `recorded` | `observed` | Assay |
| Assay | `observed` | `scored` | Countersign |
| Assay | `observed` | `screened-out` | Countersign, in case the owner overrides |
| Countersign | `scored` | `approved` | Waybill |
| Countersign | `scored` | `parked` | none |
| Countersign | `scored` | `linked` | Chartroom |
| Countersign | `scored` | `merged` | Chartroom |
| Countersign | `scored` | `rejected` | none |
| Countersign | `screened-out` | `observed` | Assay |
| Waybill | `approved` | `approved` (specification written) | Moldloft |
| Moldloft | `approved` | `building` | Shakedown |
| Shakedown | `building` | `building` (conditions passed) | Chartroom |
| Shakedown | `building` | `parked` | none |
| Chartroom | `building` | `live` | none |
| Chartroom | `linked` | `linked` (link listed) | none |
| Chartroom | `merged` | `merged` (merge listed) | none |

A worker may complete only its own rows. Tidegauge has no rows yet; see [Open questions](#open-questions).

### The five worker calls

A worker presents its Kubernetes ServiceAccount token as a bearer token. Muster asks the cluster whose token it is, and accepts a ServiceAccount in `cs-workers` whose name is the worker's registry name.

| Call | Request | Answer |
| --- | --- | --- |
| `POST /v1/lease` | `contract` | `200` with `lease`, `kind` (`item` or `proposal`), `id`, `state`, `lane`, `attempt`, `expires`, and either `results` (the latest result from each earlier worker, by worker) or `proposal`. `204` when there is nothing for this worker. |
| `POST /v1/heartbeat` | `contract`, `lease` | `200` with `lease` and the new `expires` |
| `POST /v1/complete` | `contract`, `lease`, `state`, optional `result` (a JSON object) | `200` with `id`, `state` and `next` (the worker whose step follows, if any) |
| `POST /v1/fail` | `contract`, `lease`, `code` (`E_TIMEOUT` or `E_PARSE`) | `200` with `id`, `state`, and `next` or `held` |
| `POST /v1/needs_subtool` | `contract`, `lease` | `200` with `id` and `state` |

- **Leases.** A lease lasts 60 seconds unless configured otherwise. A heartbeat moves its end to one lease length from now. When a lease runs out the step is offered again, and from then on the worker that lost it is refused. Until someone else takes the step, a late lease still counts, so a finished result that arrives late is kept.
- **Repeats.** A worker repeats a call until it is acknowledged. Repeating the call that ended a lease gives the same answer and does nothing twice.
- **`fail` with `E_TIMEOUT`.** The step stays in the queue, in its place, to be tried again.
- **`fail` with `E_PARSE`.** Work on that item stops. It keeps its state and is not offered again.
- **`needs_subtool`.** Allowed only where the state table allows it. The item moves to `needs_subtool` and leaves the queue, remembering the state it was in and the worker that paused it.
- **Replay.** When Shipkeeper reports an image digest for a worker that differs from the last digest recorded for it, every item that worker paused returns to the state it was paused from, in the `urgent` lane, in the order the items were paused. The first digest ever reported for a worker is only recorded.
- **A request body** may be at most 64 KiB.

### Errors

A refusal is an HTTP status and a body of the form `{"error": "E_PARSE", "reason": "that move is not allowed"}`. The code is always one of the three. The reason is one of a fixed list of sentences, none of which is built from request data or from an error of another system.

| Case | Status | Code |
| --- | --- | --- |
| Not JSON, an unknown field, a missing or malformed field | `400` | `E_PARSE` |
| No valid credential | `401` | `E_PARSE` |
| A caller making a call that is not its own | `403` | `E_PARSE` |
| No such call; wrong method; wrong content type; request too large | `404`, `405`, `415`, `413` | `E_PARSE` |
| A lease that is not held; a move that is not allowed | `409` | `E_PARSE` |
| A contract version or a health state Muster does not know | `422` | `E_NEEDS_SUBTOOL` |
| Too many edit submissions from one source today | `429` | `E_TIMEOUT` |
| Logbook or the identity check took too long; too many calls at once | `503` | `E_TIMEOUT` |
| A stored record that contradicts the tables (it is set aside); anything unexpected | `500` | `E_PARSE` |

The three codes do not cover every refusal exactly. Where none fits, the status says what happened and the code is the nearest of the three.

### Health reports

Shipkeeper calls `POST /v1/health` with `contract` and `reports`, a list of up to 64. A report has `component` (a registry name), `state` (`healthy`, `idle`, `degraded` or `paused`), and optionally `code` (one of the three error codes) and `digest` (`sha256:` and 64 hexadecimal digits). There is no free-text field, and a report with any other field is refused, so a report has nowhere to carry a node name or an address. A batch is kept whole or refused whole.

### Live feed

Conn subscribes with `GET /v1/feed` and receives Server-Sent Events of three kinds. Every event carries `at`, the time it was produced.

| Event | Data |
| --- | --- |
| `depths` | `urgent`, `normal`, `backfill`: steps that could be leased now. `needs_subtool`: items paused for a missing rule |
| `health` | `component`, `state`, and `code` if there is one |
| `edit` | `id` and `status` of a proposal whose status changed |

A new subscriber is first sent the current depths and the latest health of every component. After that, updates go out together in one batch every 5 seconds, never one at a time. Each batch begins with a `depths` event, so a healthy feed is never silent for more than one interval, well inside the 15 seconds after which it counts as stale. At most 8 subscribers are served at once, and one that falls behind is disconnected.

### Edit intake

Conn calls `POST /v1/edits` with `contract`, `target`, `change`, `reason`, `pow`, `honeypot` and `source` (the visitor's address). The whole request may be at most 4,096 bytes; this is checked first, because a request that is too large is never read to the end.

1. **Proof of work.** The token is `<day>.<nonce>`: the UTC date as `2026-01-05`, and up to 32 lower-case hexadecimal digits. It is valid when the SHA-256 hash of `muster-pow-v1`, `day`, `target`, `change`, `reason` and `nonce`, joined by single zero bytes, begins with at least 20 zero bits, and the day is today or yesterday in UTC. The hash covers the text, so a token cannot be reused for a different proposal.
2. **Honeypot.** If `honeypot` is not empty the answer is `204` and nothing is kept, counted or published.
3. **Rate limit.** The key for a source is HMAC-SHA-256 of its address under that day's salt. The day's salt is HMAC-SHA-256 of the UTC date under a secret created inside the cluster. At most 5 submissions a day are kept per key. Every submission that reaches this check counts, whether or not it passes the next one. The address is used for this one hash and is never stored or logged.
4. **Shape and size.** `target` must be in `ownership_map.json`. Markup is stripped from `change` and `reason`: comments, `script` and `style` elements with their contents, and anything shaped like a tag. If nothing is left of `change`, the proposal is refused.
5. **Review gate.** The proposal is kept as `pending_approval`, given an identifier of the form `EP-0001`, and queued for Countersign. The answer is `202` with `id` and `status`.

`ownership_map.json` has the form `{"elements": {"<target>": {"repository": "...", "file": "...", "kind": "DATA"}}}`. `repository` must be a repository in the naming registry and `kind` one of `DATA`, `CONFIG` and `ISSUE`.

A proposal moves through its own statuses by the same lease and complete calls as an item. A decision on a proposal carries no `result`.

| Worker | Holds a proposal in | Completes it to | Next step |
| --- | --- | --- | --- |
| Countersign | `pending_approval` | `approved` | Bellbuoy, in the `urgent` lane |
| Countersign | `pending_approval` | `declined` | none |
| Bellbuoy | `approved` | `building` | none |

### Callers other than workers

Conn and Shipkeeper present a ServiceAccount token in the same way as workers, and Muster accepts a ServiceAccount in `clearedstack` named `conn` or `shipkeeper`. Only Conn may read the feed or submit a proposal, and only Shipkeeper may report health. Without this, any pod that can reach Muster, including a worker that reads untrusted pages, could forge a health report and trigger a replay.

`GET /healthz` needs no token. It answers `200` while Muster is up, for the cluster's own checks.

## Open questions

These could not be settled from the published pages. The first build does not guess at them; the README lists each as not yet done.

1. **How an item is created.** None of the five calls records a new candidate, so nothing can put a `discovered` item into Logbook through Muster.
2. **Tidegauge's work.** Its moves are in the state table (`live` to `building` or `retired`; `parked`, `linked` or `merged` back to `observed`), but nothing says what a scheduled check is, how often one is due, or which worker follows a reopened item. Tidegauge is always told there is nothing to lease.
3. **Bellbuoy's other work.** Routing approved edits is defined above. Turning failures and paused items into tasks is not: nothing says what Bellbuoy is given.
4. **A proposal from `building` to `live`.** Nothing says who reports that an approved edit has been built and deployed.
5. **After `E_PARSE`.** A stopped item is never offered again. Nothing says how it is released once the cause is fixed.
6. **Which worker pauses a `recorded` item.** The lifecycle table says intake has no rule for the source, which points at Trawl, but Trawl holds items in `discovered`, which has no move to `needs_subtool`. The first build follows the state table: Sextant can pause a `recorded` item and Trawl cannot pause anything.
7. **Health states.** Shipkeeper's page says it sends `healthy`, `idle`, `degraded` and `paused`. The operations page lists six display states: `healthy`, `idle`, `erroring`, `recovering`, `stale` and `empty`. The first build accepts and republishes the four Shipkeeper sends, unchanged, and leaves the mapping to Conn.
8. **Duties named on other pages only.** The pipeline page says the hub rejects any message to Moldloft that carries a raw page captured by Sextant, without saying how one is recognised. Countersign's page says Muster rejects an override with no written reason. Shipkeeper's page says the owner's install, upgrade, pause and rollback actions are routed through Muster. None of these is in this specification, and none is built.
9. **The hub repository.** `ownership_map.json` and the worker contract are said to live in "the hub repository". This specification assumes that is not Muster's concern: Muster is given the ownership map as a file.

## Changes to this specification

| Change | Reason |
| --- | --- |
| Added **Contract v1**: states and moves, lanes, queue steps, the five calls, errors, health reports, the live feed and edit intake | The specification named these and did not define them. A worker cannot be written against a name, and a rule with no definition cannot have a saved example |
| Followed the lifecycle page's table over its figure | They disagree, and the table is the more detailed of the two |
| Mapped every refusal to an HTTP status plus the nearest of the three codes | Only three public codes are allowed, and an illegal move, an expired lease and a missing credential fit none of them exactly |
| Required a ServiceAccount token from Conn and Shipkeeper as well as from workers | The interface table lists authentication for workers only, which would let any pod that can reach Muster forge a health report or a proposal |
| Added `GET /healthz` | A pod needs something the cluster can check |
| Added **Open questions** | To record what the first build refuses to guess |

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
