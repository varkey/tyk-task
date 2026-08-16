← [README](README.md) · assumes you've completed [deploy.md](deploy.md)
(deployed with `make helm-install-no-auth`, so none of the calls below need a
bearer token) and have a port-forward open on `localhost:18080`.

# Demo

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

## Story 3 - API server connectivity

```sh
curl localhost:18080/readyz
# {"kubernetesVersion":"v1.36.1","ready":true}
```

To see this live, cut the tool's own egress to the API server with a plain
NetworkPolicy - the same primitive story 2 manages, applied directly here
since the API server isn't a workload story 2's namespace/label selectors
can target:

```sh
kubectl --context kind-tyk-sre-assignment -n default apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cut-api-access
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: tyk-sre-assignment
      app.kubernetes.io/instance: tyk-sre-assignment
  policyTypes:
    - Egress
  egress:
    - ports: # DNS only - everything else, including the API server, is denied
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
EOF
```

Applying it alone shows nothing yet - the same limitation as story 2's
isolation (see the README's [Known limitations](README.md#known-limitations)):
the pod's existing connection to the API server survives a policy change
that would now deny it.

```sh
curl localhost:18080/readyz
# still {"kubernetesVersion":"v1.36.1","ready":true} - the existing connection is unaffected
```

Prove the policy is actually enforced, without touching the pod at all, by
making a genuinely new connection from inside it: `kubectl debug` attaches a
throwaway container sharing the pod's network namespace, so this doesn't
require curl (or a shell) in the shipped image. `kubernetes.default.svc` is
the API server's own in-cluster DNS name - it resolves to whatever ClusterIP
the cluster actually assigned, unlike a hardcoded IP:

```sh
POD=$(kubectl --context kind-tyk-sre-assignment -n default get pod -l app.kubernetes.io/name=tyk-sre-assignment -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-tyk-sre-assignment -n default debug "$POD" --image=curlimages/curl --target=tyk-sre-assignment -- curl -sk --max-time 5 https://kubernetes.default.svc/version
# times out - a fresh connection is blocked immediately, unlike the app's own long-lived one
```

To see `/readyz` itself flip, restart the pod so the app has to make a fresh
connection:

```sh
kubectl --context kind-tyk-sre-assignment -n default delete pod -l app.kubernetes.io/name=tyk-sre-assignment

# Comes up at 0/1 and stays there (no crash-loop):
kubectl --context kind-tyk-sre-assignment -n default get pods -l app.kubernetes.io/name=tyk-sre-assignment
# 0/1  Running

curl localhost:18080/readyz
# {"ready":false,"error":"...context deadline exceeded..."}
```

`kubectl logs` marks the start of the outage with one line - not one per
failed probe, and not silence either:

```
2026/08/16 12:09:02 readyz: k8s API server unreachable: Get "https://10.96.0.1:443/version?timeout=5s": context deadline exceeded
```

Restore it - no further restart needed, the very next `/readyz` call
succeeds as soon as the policy's gone:

```sh
kubectl --context kind-tyk-sre-assignment -n default delete networkpolicy cut-api-access

curl localhost:18080/readyz
# {"kubernetesVersion":"v1.36.1","ready":true}

kubectl --context kind-tyk-sre-assignment -n default get pods -l app.kubernetes.io/name=tyk-sre-assignment
# 1/1  Running, once the readiness probe catches up (a few seconds)
```

...with a matching bookend in the logs, again exactly once:

```
2026/08/16 12:10:50 readyz: k8s API server reachable again
```

(Re-open the port-forward from the top of this doc if the pod restart
dropped it.)

**Stories 4 & 5** aren't things you curl - see the README's CI/CD section and
[deploy.md](deploy.md).

## Auth

Everything above was run with auth disabled (`make helm-install-no-auth`) for
a frictionless walkthrough. In a real deployment `auth.enabled=true` is the
chart's default - every `/api/v1/*` call then needs `Authorization: Bearer
<token>`. The token is checked two ways: `TokenReview` resolves who it
belongs to, `SubjectAccessReview` checks whether *that identity's own RBAC*
permits the specific action being requested (see the README's Decisions &
tradeoffs section for why).

Redeploy with auth on, then re-open the port-forward from the top of this doc:

```sh
make helm-install
```

```sh
kubectl --context kind-tyk-sre-assignment create serviceaccount sre-caller
kubectl --context kind-tyk-sre-assignment create serviceaccount no-access

# sre-caller can list deployments cluster-wide and manage networkpolicies in ns-a/ns-b.
kubectl --context kind-tyk-sre-assignment create clusterrole demo-deployments --verb=list --resource=deployments
kubectl --context kind-tyk-sre-assignment create clusterrolebinding demo-deployments --clusterrole=demo-deployments --serviceaccount=default:sre-caller
kubectl --context kind-tyk-sre-assignment create role demo-netpol -n ns-a --verb=create,delete --resource=networkpolicies
kubectl --context kind-tyk-sre-assignment create rolebinding demo-netpol -n ns-a --role=demo-netpol --serviceaccount=default:sre-caller
kubectl --context kind-tyk-sre-assignment create role demo-netpol -n ns-b --verb=create,delete --resource=networkpolicies
kubectl --context kind-tyk-sre-assignment create rolebinding demo-netpol -n ns-b --role=demo-netpol --serviceaccount=default:sre-caller

SRE_TOKEN=$(kubectl --context kind-tyk-sre-assignment create token sre-caller)
NOACCESS_TOKEN=$(kubectl --context kind-tyk-sre-assignment create token no-access)

curl -o /dev/null -w '%{http_code}\n' localhost:18080/api/v1/deployments/health                            # 401 - no token
curl -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $NOACCESS_TOKEN" localhost:18080/api/v1/deployments/health  # 403 - authenticated, no RBAC
curl -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $SRE_TOKEN" localhost:18080/api/v1/deployments/health       # 200 - has RBAC
```
