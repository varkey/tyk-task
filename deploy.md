← [README](README.md)

# Deploying

Prerequisites: `go`, `docker`, `kind`, `helm`, `kubectl`.

```sh
# 1. Cluster with kind's default CNI disabled, then Calico installed instead.
#    This matters: kind's default CNI (kindnet) does not enforce
#    NetworkPolicy at all. Skip this and story 2's isolation will create the
#    right objects but never actually block anything - see the README's
#    Decisions & tradeoffs section.
make kind-create
make calico-install

# 2. Build the image, load it into kind, deploy the chart.
make helm-install

# 3. A couple of namespaces/workloads to demo stories 1 and 2 against.
make demo-workloads

kubectl get pods -A
```

By default the chart deploys with `auth.enabled=true`, which needs a bearer
token for every `/api/v1/*` call (see [demo.md](demo.md#auth) for how to mint
one). For the fastest path through the demo, redeploy with auth off instead:

```sh
helm upgrade --install tyk-sre-assignment chart \
  --namespace default --kube-context kind-tyk-sre-assignment \
  --set image.repository=tyk-sre-assignment --set image.tag=dev \
  --set auth.enabled=false
```

Port-forward once, then run the calls in [demo.md](demo.md) against
`localhost:18080`:

```sh
kubectl --context kind-tyk-sre-assignment -n default port-forward svc/tyk-sre-assignment 18080:8080
```

Next: [demo.md](demo.md) walks through exercising all five stories.
