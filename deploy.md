← [README](README.md)

# Deploying

Prerequisites: `kind`, `helm`, `kubectl`. (`go` and `docker` are only needed
for `LOCAL=1` below - building from source rather than deploying CI's
published image.)

```sh
# 1. Cluster with kind's default CNI disabled, then Calico installed instead.
#    This matters: kind's default CNI (kindnet) does not enforce
#    NetworkPolicy at all. Skip this and story 2's isolation will create the
#    right objects but never actually block anything - see the README's
#    Decisions & tradeoffs section.
make kind-create
make calico-install

# 2. Deploy the chart - auth disabled, for the fastest path through demo.md
#    (no bearer tokens to mint just to try the tool out). No build required:
#    this pulls the image CI already published to ghcr.io/varkey/tyk-task,
#    tagged `latest`. To try out local changes instead, add LOCAL=1 (e.g.
#    `make helm-install-no-auth LOCAL=1`) to build from this checkout and
#    load that image into kind instead.
make helm-install-no-auth

# 3. A couple of namespaces/workloads to demo stories 1 and 2 against.
make demo-workloads

kubectl get pods -A
```

The chart defaults to `auth.enabled=true` in a real deployment - every
`/api/v1/*` call then needs a bearer token, checked via TokenReview +
SubjectAccessReview (see the README's Decisions & tradeoffs section for why).
To see that in action instead, deploy with auth on and follow
[demo.md's Auth section](demo.md#auth) for how to mint a token:

```sh
make helm-install
```

Port-forward once, then run the calls in [demo.md](demo.md) against
`localhost:18080`:

```sh
kubectl --context kind-tyk-sre-assignment -n default port-forward svc/tyk-sre-assignment 18080:8080
```

Next: [demo.md](demo.md) walks through exercising all five stories.
