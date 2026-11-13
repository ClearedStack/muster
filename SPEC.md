# Muster specification

> Central queue lease manager, state-transition gatekeeper, SSE live feed publisher, and 5-layer anti-spam edit intake in the clearedstack namespace.

**Status: design.** This is a specification. It describes how Muster is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/muster/](https://clearedstack.com/platform/muster/). To change the specification, change it here and say why.

Muster (`v0.1.0-draft`, owned by `clearedstack.com`) is the sole read-write gatekeeper in front of Logbook. It coordinates all 10 deterministic workers in `cs-workers`, streams live status to Conn over Server-Sent Events (SSE), and protects anonymous edit intake with a 5-layer anti-spam shield.

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
| **Owning Repo** | `clearedstack.com` |
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

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
