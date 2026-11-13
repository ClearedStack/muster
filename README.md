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

> **Status: design.** Nothing is built yet. This repository holds the specification and the prompt for building it, and no code.

Muster is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Muster is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Muster does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
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

## Names

`muster` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
