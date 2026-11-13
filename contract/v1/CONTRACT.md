# Muster contract v1

This is the contract between Muster and everything that calls it: the ten workers, Conn and Shipkeeper. It defines the calls, the states, the queue and the refusals. [`SPEC.md`](../../SPEC.md) says what Muster is for; this file says exactly what it does.

Every call is JSON over cluster-private HTTP. Every request body carries `"contract": "v1"`. A body with a field the call does not define is refused.

[`examples/`](examples) holds one saved example for each call and each refusal: the request, without the token, and the exact status and body expected back. A test replays every one of them against Muster running on its in-memory stand-in for Logbook.

A test also checks the tables under **Item states and moves**, **Queue steps** and **Proposal steps** against the code. If one is changed without the other, the test fails.

## Identifiers

| What | Form | Examples |
| --- | --- | --- |
| An item | `^PA-([0-9]{4}\|[1-9][0-9]{4,8})$` | `PA-0001`, `PA-0417`, `PA-10000` |
| An edit proposal | `^EP-([0-9]{4}\|[1-9][0-9]{4,8})$` | `EP-0001`, `EP-10000` |

That is four digits, padded with zeros, or five to nine digits with no leading zero. Muster hands out the numbers in order.

## Item states and moves

An item has exactly one of fourteen states. The states and moves are the table "Which worker makes each move" on the [item lifecycle page](https://clearedstack.com/platform/item-lifecycle/). Where that page's figure and its table disagree, the table is followed.

| From | May move to |
| --- | --- |
| `discovered` | `recorded`, `rejected` |
| `recorded` | `observed`, `needs_subtool` |
| `observed` | `scored`, `screened_out`, `needs_subtool` |
| `screened_out` | `observed` |
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

## Lanes

Every queue step is in one of three lanes. A worker is offered the `urgent` lane first, then `normal`, then `backfill`, and within a lane the step that arrived first.

## Queue steps

The queue holds, for each item, at most one step of its own: the next thing to do, and which worker does it. Completing a step changes the state and writes the next step in one transaction. Which worker follows is fixed by this table.

| Worker | Holds an item in | Completes it to | Next step |
| --- | --- | --- | --- |
| Trawl | `discovered` | `recorded` | Sextant |
| Trawl | `discovered` | `rejected` | none |
| Sextant | `recorded` | `observed` | Assay |
| Assay | `observed` | `scored` | Countersign |
| Assay | `observed` | `screened_out` | Countersign, in case the owner overrides |
| Countersign | `scored` | `approved` | Waybill |
| Countersign | `scored` | `parked` | none |
| Countersign | `scored` | `linked` | Chartroom |
| Countersign | `scored` | `merged` | Chartroom |
| Countersign | `scored` | `rejected` | none |
| Countersign | `screened_out` | `observed` | Assay |
| Waybill | `approved` | `approved` (specification written) | Moldloft |
| Moldloft | `approved` | `building` | Shakedown |
| Shakedown | `building` | `building` (conditions passed) | Chartroom |
| Shakedown | `building` | `parked` | none |
| Chartroom | `building` | `live` | Tidegauge, in the `backfill` lane, 7 days later |
| Chartroom | `linked` | `linked` (link listed) | none |
| Chartroom | `merged` | `merged` (merge listed) | none |
| Chartroom | `retired` | `retired` (retirement listed) | none |
| Tidegauge | `live` | `live` (checked, nothing changed) | Tidegauge, in the `backfill` lane, 7 days later |
| Tidegauge | `live` | `retired` | Chartroom, in the `urgent` lane |

A worker may complete only its own rows. A step marked "7 days later" is not offered, and is not counted as waiting, until seven days have passed. That is how every live item comes back to Tidegauge for a check every seven days.

## Who may call what

Every caller presents its Kubernetes ServiceAccount token as a bearer token. Muster asks the cluster whose token it is. It accepts a ServiceAccount in `cs-workers` named after a worker, or one in `clearedstack` named `conn` or `shipkeeper`.

| Call | Who |
| --- | --- |
| `POST /v1/lease`, `/v1/heartbeat`, `/v1/complete`, `/v1/fail`, `/v1/needs_subtool` | any of the ten workers |
| `POST /v1/items` | `trawl`, `conn` |
| `POST /v1/release` | `countersign` |
| `POST /v1/health` | `shipkeeper` |
| `POST /v1/edits`, `GET /v1/feed` | `conn` |
| `GET /healthz` | anyone; no token |

## The five worker calls

| Call | Request | Answer |
| --- | --- | --- |
| `POST /v1/lease` | `contract` | `200` with `lease`, `kind`, `id`, `state`, `lane`, `attempt`, `expires`, and what the step is about: `found` and `results` for an item, `proposal` for a proposal, `incident` for an incident. `204` when there is nothing for this worker. |
| `POST /v1/heartbeat` | `contract`, `lease` | `200` with `lease` and the new `expires` |
| `POST /v1/complete` | `contract`, `lease`, `state`, optional `result` (a JSON object) | `200` with `id`, `state` and `next` (the worker whose step follows, if any) |
| `POST /v1/fail` | `contract`, `lease`, `code` (`E_TIMEOUT` or `E_PARSE`) | `200` with `id`, `state`, and `next` or `held` |
| `POST /v1/needs_subtool` | `contract`, `lease` | `200` with `id` and `state` |

- **Kinds.** `kind` is `item`, `proposal` or `incident`, and says what the step is about.
- **Found.** A lease on an item that was recorded through `POST /v1/items` carries `found`: its `source`, `title` and `address`, as they were recorded.
- **Results.** Each worker's result is kept under that worker's name, and every later worker is given all of them: a lease on an item carries `results.trawl`, `results.sextant` and so on. A worker that completes a second step on the same item replaces its own earlier result and no one else's.
- **Leases.** A lease lasts 60 seconds unless configured otherwise. A heartbeat moves its end to one lease length from now. When a lease runs out the step is offered again, and from then on the worker that lost it is refused. Until someone else takes the step, a late lease still counts, so a finished result that arrives late is kept.
- **Repeats.** A worker repeats a call until it is acknowledged. Repeating the call that ended a lease gives the same answer and does nothing twice.
- **`fail` with `E_TIMEOUT`.** The step stays in the queue, in its place, to be tried again.
- **`fail` with `E_PARSE`.** Work on that item stops. It keeps its state and is not offered again until the owner releases it. Bellbuoy is given an incident step.
- **`needs_subtool`.** Allowed only where the state table allows it. The item moves to `needs_subtool` and leaves the queue, remembering the state it was in and the worker that paused it. Bellbuoy is given an incident step.
- **Replay.** When Shipkeeper reports an image digest for a worker that differs from the last digest recorded for it, every item that worker paused returns to the state it was paused from, in the `urgent` lane, in the order the items were paused. The first digest ever reported for a worker is only recorded.
- **Size.** A request body may be at most 64 KiB. The one exception is `complete` from Waybill, which carries a whole specification and may be at most 256 KiB.

## Incident steps

An item that is stopped by `E_PARSE` or paused by `needs_subtool` gets one incident step for Bellbuoy, in the `normal` lane. Bellbuoy leases it like any other step. The lease has `kind` `incident` and carries:

```json
"incident": {"id": "PA-0001", "worker": "sextant", "code": "E_PARSE", "state": "recorded"}
```

`id` is the item, `worker` is the worker that stopped, `code` is `E_PARSE` or `E_NEEDS_SUBTOOL`, and `state` is the state the item was in when the worker stopped. For a paused item that is the state it was paused from.

An incident step changes no state. Bellbuoy completes it with the same `state` it was given and no `result`. Anything else is refused. An item has at most one incident step: if it is stopped again before Bellbuoy has dealt with the first, the newer incident takes its place.

An item whose stored record contradicts the tables is set aside in the same way, with `code` `E_PARSE`, and gets an incident step too.

## Release

`POST /v1/release` with `contract` and `id` carries the owner's decision to let a stopped item run again. Countersign makes the call. The item keeps the state it was stopped in, and its step goes back to the worker it was for, at the end of the `urgent` lane. The answer is `200` with `id`, `state` and `next`. An item that is not stopped is refused with `move_not_allowed`.

## Recording a candidate

`POST /v1/items` records a find as an item, once.

| Field | Meaning |
| --- | --- |
| `contract` | `v1` |
| `source` | where it was found: `sam-gov`, `github-issues`, `web-sweep` or `site-form` |
| `title` | at most 200 characters, one line; markup is stripped |
| `address` | at most 2,000 characters, with no white space |
| `result` | optional JSON object; kept as the caller's result, `results.trawl` or `results.conn` |
| `pow`, `visitor` | only with `site-form`, and required with it: the puzzle token and the visitor's address |

- **A new address.** `201` with `{"id": "PA-0001", "state": "discovered", "next": "trawl"}`. The item starts in the `backfill` lane when the source is `web-sweep`, and in `normal` otherwise.
- **A known address.** `200` with `{"id": "PA-0001", "state": "observed", "duplicate": true}`: the item that already has the address and its state now. Nothing changes but the ledger, which gets a row for the sighting.
- **Comparing addresses.** Addresses are compared exactly as sent. Making equivalent addresses equal is Trawl's rule, not Muster's.
- **`site-form`.** A candidate from the form on the site passes the same proof of work as an edit proposal, with `source`, `title` and `address` in the place of `target`, `change` and `reason`, and counts against the same daily limit as that visitor's edit proposals. The visitor's address is used for the one hash and is never stored.
- **A source Muster does not know** is valid and unfamiliar: `422`, `E_NEEDS_SUBTOOL`.

## Refusals

A refusal is an HTTP status and a body with exactly three fields:

```json
{"error": "E_PARSE", "cause": "move_not_allowed", "reason": "that move is not allowed"}
```

`error` is one of the three public codes. `cause` is a fixed word a program can act on. `reason` is a fixed sentence for a person. Neither is ever built from request data or from an error of another system.

| Status | `cause` | `error` | When |
| --- | --- | --- | --- |
| `400` | `not_json` | `E_PARSE` | The body is not one JSON value |
| `400` | `unknown_field` | `E_PARSE` | The body has a field the call does not define |
| `400` | `missing_field` | `E_PARSE` | A required field is missing or empty |
| `400` | `bad_field` | `E_PARSE` | A field has the wrong type or form: a value that is not allowed, an invalid proof of work, a target that is not in the ownership map, text with nothing left once markup is removed |
| `401` | `no_credential` | `E_PARSE` | No valid token |
| `403` | `not_your_call` | `E_PARSE` | A caller making a call that is not its own |
| `404` | `no_such_call` | `E_PARSE` | No such path |
| `405` | `wrong_method` | `E_PARSE` | The wrong HTTP method |
| `413` | `too_large` | `E_PARSE` | The body is over its limit |
| `415` | `wrong_type` | `E_PARSE` | The content type is not `application/json` |
| `409` | `lease_not_held` | `E_PARSE` | The lease is unknown, has ended, or belongs to another worker |
| `409` | `move_not_allowed` | `E_PARSE` | The tables do not allow the move, or it is not this worker's to make |
| `409` | `duplicate` | `E_PARSE` | One batch of health reports names the same component twice |
| `422` | `unknown_contract` | `E_NEEDS_SUBTOOL` | A contract version Muster does not know |
| `422` | `unknown_value` | `E_NEEDS_SUBTOOL` | A health state or a source Muster does not know |
| `429` | `rate_limited` | `E_TIMEOUT` | Too many submissions from one visitor today |
| `503` | `store_slow` | `E_TIMEOUT` | Logbook took too long |
| `503` | `busy` | `E_TIMEOUT` | Too many calls at once, or the identity check took too long |
| `500` | `bad_record` | `E_PARSE` | A stored record contradicts the tables; it has been set aside |
| `500` | `unexpected` | `E_PARSE` | Anything else |

The three codes do not cover every refusal exactly. Where none fits, the status and the cause say what happened and the code is the nearest of the three.

## Health reports

Shipkeeper calls `POST /v1/health` with `contract` and `reports`, a list of up to 64. A report has `component` (a registry name), `state` (`healthy`, `idle`, `degraded` or `paused`), and optionally `code` (one of the three error codes) and `digest` (`sha256:` and 64 hexadecimal digits). There is no free-text field, and a report with any other field is refused, so a report has nowhere to carry a node name or an address. A batch is kept whole or refused whole.

The answer is `200` with `received`, `replayed` and `live`.

A report is also how Muster learns that a component was redeployed. When the digest reported for a component differs from the last one recorded for it:

- if the component is a worker, every item it paused is replayed, and `replayed` counts them;
- every edit proposal that is `building` in that component's repository moves to `live`, and `live` counts them.

The first digest ever reported for a component is only recorded.

## Live feed

Conn subscribes with `GET /v1/feed` and receives Server-Sent Events of three kinds. Every event carries `at`, the time it was produced.

| Event | Data |
| --- | --- |
| `depths` | `urgent`, `normal`, `backfill`: steps that could be leased now. `needs_subtool`: items paused for a missing rule |
| `health` | `component`, `state`, and `code` if there is one |
| `edit` | `id` and `status` of a proposal whose status changed |

A new subscriber is first sent the current depths and the latest health of every component. After that, updates go out together in one batch every 5 seconds, never one at a time. Each batch begins with a `depths` event, so a healthy feed is never silent for more than one interval, well inside the 15 seconds after which it counts as stale. At most 8 subscribers are served at once, and one that falls behind is disconnected.

## Edit intake

Conn calls `POST /v1/edits` with `contract`, `target`, `change`, `reason`, `pow`, `honeypot` and `source` (the visitor's address).

1. **Proof of work.** The token is `<day>.<nonce>`: the UTC date as `2026-01-05`, and up to 32 lower-case hexadecimal digits. It is valid when the SHA-256 hash of `muster-pow-v1`, `day`, `target`, `change`, `reason` and `nonce`, joined by single zero bytes, begins with at least 20 zero bits, and the day is today or yesterday in UTC. The hash covers the text, so a token cannot be reused for a different proposal.
2. **Honeypot.** If `honeypot` is not empty the answer is `204` and nothing is kept, counted or published.
3. **Rate limit.** The key for a visitor is HMAC-SHA-256 of the address under that day's salt. The day's salt is HMAC-SHA-256 of the UTC date under a secret created inside the cluster. At most 5 submissions a day are kept per key. Every submission that reaches this check counts, whether or not it passes the next one. The address is used for this one hash and is never stored or logged.
4. **Shape and size.** The whole request may be at most 4,096 bytes. `target` must be in `ownership_map.json`. Markup is stripped from `change` and `reason`: comments, `script` and `style` elements with their contents, and anything shaped like a tag. If nothing is left of `change`, the proposal is refused.
5. **Review gate.** The proposal is kept as `pending_approval`, given the next identifier, and queued for Countersign. The answer is `202` with `id` and `status`.

`ownership_map.json` has the form `{"elements": {"<target>": {"repository": "...", "file": "...", "kind": "DATA"}}}`. `repository` must be a repository in the naming registry and `kind` one of `DATA`, `CONFIG` and `ISSUE`.

## Proposal steps

A proposal moves through its own statuses by the same lease and complete calls as an item. A decision on a proposal carries no `result`.

| Worker | Holds a proposal in | Completes it to | Next step |
| --- | --- | --- | --- |
| Countersign | `pending_approval` | `approved` | Bellbuoy, in the `urgent` lane |
| Countersign | `pending_approval` | `declined` | none |
| Bellbuoy | `approved` | `building` | none |

The last move, `building` to `live`, is not a worker's. It happens when Shipkeeper reports a new digest for the proposal's repository; see **Health reports**.

## The check the cluster makes

`GET /healthz` needs no token. It answers `200` with `{"status": "ok"}` while Muster is up, and is answered even when Muster is too busy for anything else.
