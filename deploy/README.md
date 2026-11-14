# Running Muster in a cluster

`deploy/` is a kustomize directory, written for Argo CD.

| File | What it applies |
| --- | --- |
| `muster.yaml` | ServiceAccount `muster`, its binding to the cluster role `system:auth-delegator`, the ownership map as a ConfigMap, the Deployment and the Service on 8080, all in the namespace `clearedstack` |
| `networkpolicy.yaml` | Who may reach Muster, and what Muster may reach |
| `workers.yaml` | The namespace `cs-workers`, closed by default, with the one rule that lets a worker call Muster |

Whoever deploys it supplies four things that are not in this repository:

- **The image.** `muster.yaml` names the image `muster`. Build it with `ko build --platform=linux/arm64 --bare ./cmd/muster` and give the registry and digest as a kustomize image override.
- **Secret `muster-database`**, key `url`: where Logbook is, as a PostgreSQL address for the `muster` role.
- **Secret `muster-salt`**, key `salt`: at least 32 random bytes. Make it once and keep it: the daily rate limit is counted against it.
- **A rule that lets Muster reach the cluster's API**, which a NetworkPolicy here cannot name.

Logbook has to let Muster in as well: its own `logbook-ingress` policy lists who may connect.

Muster runs as an ordinary user with a read-only root filesystem, no capabilities and 48Mi of memory. A caller is a pod of a component that may call Muster, presenting the token of a ServiceAccount named after that component, in that component's namespace: `assay` in `cs-workers`, `conn` in `clearedstack`.
