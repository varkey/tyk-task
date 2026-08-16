← [README](README.md) · assumes you've completed [deploy.md](deploy.md) and
have a port-forward open on `localhost:18080`.

# Demo

## Story 3 - connectivity

```sh
curl localhost:18080/readyz
# {"kubernetesVersion":"v1.36.1","ready":true}
```

Kill the tool's route to the API server (e.g. scale it down while blocking
egress, or just watch `kubectl get pods` - Ready flips to `False` the moment
`/readyz` starts failing, since it's wired as the readiness probe) to see
the other side of this.

## Story 1 - deployment health

`demo-workloads` includes a `broken` Deployment (bad image, asks for 3
replicas, gets 0) alongside `frontend`/`backend`, which are healthy:

```sh
curl -s localhost:18080/api/v1/deployments/health | jq
# "allHealthy": false, "unhealthyCount": 1, and "broken" sorts first in
# "deployments" with desiredReplicas: 3, readyReplicas: 0, healthy: false

curl -s localhost:18080/api/v1/deployments/health?onlyUnhealthy=true | jq '.deployments'
# just the "broken" entry - useful for scripts that want the trouble list without jq filtering
```

## Story 2 - network isolation

```sh
# Baseline: frontend can reach backend.
FRONTEND=$(kubectl --context kind-tyk-sre-assignment -n ns-a get pod -l app=frontend -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-tyk-sre-assignment -n ns-a exec "$FRONTEND" -- wget -qT5 -O- backend.ns-b.svc.cluster.local | head -1

# Cut them off.
curl -s -X POST localhost:18080/api/v1/isolation -H 'Content-Type: application/json' -d '{
  "a": {"namespaces": ["ns-a"], "matchLabels": {"app": "frontend"}},
  "b": {"namespaces": ["ns-b"], "matchLabels": {"app": "backend"}}
}' | tee /tmp/iso.json | jq

# Confirm it's actually blocked (this exec should now time out).
kubectl --context kind-tyk-sre-assignment -n ns-a exec "$FRONTEND" -- wget -qT5 -O- backend.ns-b.svc.cluster.local

# Reverse it.
ID=$(jq -r .id /tmp/iso.json)
curl -s -X DELETE localhost:18080/api/v1/isolation/$ID -o /dev/null -w '%{http_code}\n'

# Restored.
kubectl --context kind-tyk-sre-assignment -n ns-a exec "$FRONTEND" -- wget -qT5 -O- backend.ns-b.svc.cluster.local | head -1
```

`GET /api/v1/isolation` lists every isolation currently applied (reconstructed
from the NetworkPolicy objects themselves - there's no separate database).

**Stories 4 & 5** aren't things you curl - see the README's CI/CD section and
[deploy.md](deploy.md).

## Auth

With `auth.enabled=true` (the chart's default), every `/api/v1/*` call needs
`Authorization: Bearer <token>`. The token is checked two ways: `TokenReview`
resolves who it belongs to, `SubjectAccessReview` checks whether *that
identity's own RBAC* permits the specific action being requested (see the
README's Decisions & tradeoffs section for why).

```sh
kubectl create serviceaccount sre-caller
kubectl create serviceaccount no-access

# sre-caller can list deployments cluster-wide and manage networkpolicies in ns-a/ns-b.
kubectl create clusterrole demo-deployments --verb=list --resource=deployments
kubectl create clusterrolebinding demo-deployments --clusterrole=demo-deployments --serviceaccount=default:sre-caller
kubectl create role demo-netpol -n ns-a --verb=create,delete --resource=networkpolicies
kubectl create rolebinding demo-netpol -n ns-a --role=demo-netpol --serviceaccount=default:sre-caller
kubectl create role demo-netpol -n ns-b --verb=create,delete --resource=networkpolicies
kubectl create rolebinding demo-netpol -n ns-b --role=demo-netpol --serviceaccount=default:sre-caller

SRE_TOKEN=$(kubectl create token sre-caller)
NOACCESS_TOKEN=$(kubectl create token no-access)

curl -o /dev/null -w '%{http_code}\n' localhost:18080/api/v1/deployments/health                            # 401 - no token
curl -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $NOACCESS_TOKEN" localhost:18080/api/v1/deployments/health  # 403 - authenticated, no RBAC
curl -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $SRE_TOKEN" localhost:18080/api/v1/deployments/health       # 200 - has RBAC
```

All of the above was run against a real kind+Calico cluster while writing
this, not just asserted in unit tests.
