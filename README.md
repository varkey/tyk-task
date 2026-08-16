# tyk-sre-assignment

An internal SRE tool for a Kubernetes cluster: reports Deployment replica
health, checks its own connectivity to the API server, and can cut off
network traffic between two workloads on demand. Originally cloned from
[TykTechnologies/tyk-sre-assignment](https://github.com/TykTechnologies/tyk-sre-assignment);
this repo (`varkey/tyk-task`) extends that starting point in place.

It's a single Go binary, one HTTP server, one container image, one Helm
chart. No UI - see [Decisions & tradeoffs](#decisions--tradeoffs).

## The five stories

| # | Story | How |
|---|-------|-----|
| 1 | Are all Deployments as healthy as their spec asks for? | `GET /api/v1/deployments/health` |
| 2 | Cut off network traffic between two workloads, on demand | `POST` / `DELETE /api/v1/isolation` |
| 3 | Always know if this tool can reach the API server | `GET /readyz`, wired as the Deployment's readiness probe |
| 4 | Build a container image on push to `main` | `.github/workflows/ci.yml` |
| 5 | Deploy via Helm | `chart/` |

## Getting started

- **[deploy.md](deploy.md)** - deploy against a local kind cluster
- **[demo.md](demo.md)** - exercise all five stories, plus the auth layer,
  against that deployment

## API reference

All bodies are JSON.

| Method | Path | Auth check | Notes |
|---|---|---|---|
| GET | `/healthz` | none | liveness only, never touches the API server |
| GET | `/readyz` | none | live connectivity check; 503 when unreachable |
| GET | `/api/v1/deployments/health?namespace=&onlyUnhealthy=` | `list deployments` (cluster-wide, or in `namespace` if given) | `deployments` is sorted unhealthy-first; `onlyUnhealthy=true` trims it to just the unhealthy entries, `totalDeployments`/`healthyCount`/`unhealthyCount` always reflect the full set regardless |
| GET | `/api/v1/isolation` | `list networkpolicies` | |
| POST | `/api/v1/isolation` | `create networkpolicies` in every namespace named in the body | body: `{"a": {"namespaces": [...], "matchLabels": {...}}, "b": {...}}` |
| DELETE | `/api/v1/isolation/{id}` | `delete networkpolicies` in every namespace the isolation touches | 404 if `id` doesn't exist |

## Testing

```sh
cd golang && go test ./... -race -cover
```

Current coverage: `internal/k8shealth` 100%, `internal/authn` 95%,
`internal/api` 85%, `internal/netpolicy` 90%.

## CI/CD

`.github/workflows/ci-golang.yml` and `.github/workflows/ci-chart.yml`, on
every push/PR:

- **test** - `go build`, `go vet`, `go test -race -cover`
- **lint** - `golangci-lint`
- **helm** - `helm lint` + `helm template` under a few different value sets
  (default, namespaced RBAC, auth disabled)
- **build-image** - builds the Dockerfile always; only pushes to
  `ghcr.io/varkey/tyk-task` on pushes to `main`, tagged with the commit SHA
  and `latest`

## Decisions & tradeoffs

**Service vs. CLI.** The stories, taken as a whole, push this toward being
an in-cluster service rather than a CLI - story 3 wants continuous "always
know" awareness, and the requirements bundle in a container image and a
Helm chart. That said, I think a CLI would genuinely suit some of these
stories better - it inherits the operator's own kubeconfig and RBAC for
free, no server, no auth to build.

**Network isolation.** This only works if the cluster's CNI actually
enforces NetworkPolicy. kind's default CNI (kindnet) doesn't, which is why
[deploy.md](deploy.md) installs Calico first - without it, the isolation API
would create the right objects but it wouldn't be enforced.

**Symmetric isolation policies.** Creating an isolation writes a NetworkPolicy for both
workloads in a pair, for thoroughness. A single policy - Ingress+Egress on
either workload's own selector - already blocks traffic in both directions. 

**Auth.** The user stories do not ask for auth, but an unauthenticated
endpoint that can create or delete NetworkPolicies (or read every
Deployment in the cluster) isn't defensible by any normal security bar, so
the auth is necessary here. It uses Kubernetes' own built-in access
mechanisms - TokenReview to authenticate the caller, then
SubjectAccessReview to check that caller's own RBAC for the specific action
they're asking for - so there's no separate token or secret to manage.
`--auth-enabled` /
`auth.enabled` is only used to disable auth for testing - it's on by
default.

**RBAC scope.** Story 1 asks about all the deployments in the cluster, and
story 2 needs to isolate workloads across arbitrary namespace pairs - both
are inherently cross-namespace, so the ServiceAccount gets a
ClusterRole/ClusterRoleBinding rather than a namespace-scoped Role. However,
the chart supports targeting the service's scope to a specific list of
namespaces if required, in which case Role/RoleBinding pairs are created in
each namespace instead - a Role never authorizes a cluster-wide list no
matter how many namespaces it covers, so that mode also tells the app the
same namespace list (`--namespaces`) and it iterates per namespace instead
of trying one cluster-wide call.

**No UI.** The user stories do not ask specifically ask for a UI or a
dashboard, so I didn't build one. The JSON API with curl would satisfy the
requirements as stated.

## Known limitations

**Isolation doesn't cut off a connection already open and in active use.**
Like Kubernetes NetworkPolicy enforcement generally, Calico only checks
policy against the first packet of a flow - conntrack lets an established
connection keep flowing after a policy change that would now deny it, until
that connection closes or goes idle (see Calico's [connection tracking
docs](https://docs.tigera.io/calico/latest/reference/host-endpoints/conntrack)
and [projectcalico/calico#6399](https://github.com/projectcalico/calico/issues/6399),
an open ask for a way to force-close matching connections on policy change).
New connections are blocked the instant `POST /api/v1/isolation` is called;
an already-open one keeps working until it ends on its own.