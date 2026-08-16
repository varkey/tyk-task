# Matches chart/values.yaml's image.repository - CI publishes here
# (ghcr.io/<owner>/<repo>, lowercased) on pushes to main, tagged with the
# commit SHA and `latest`.
IMAGE_REPO    ?= ghcr.io/varkey/tyk-task
KIND_CLUSTER  ?= tyk-sre-assignment
KIND_CONTEXT  := kind-$(KIND_CLUSTER)
CHART         := chart
RELEASE       ?= tyk-sre-assignment
NAMESPACE     ?= default
CALICO_VERSION ?= v3.29.1

# LOCAL=1 (e.g. `make helm-install LOCAL=1`) builds the image from this
# checkout and loads it into kind instead of deploying what CI already
# published - for trying out changes that haven't been pushed to main yet.
# Deploying otherwise never needs a local build: it just pulls
# $(IMAGE_REPO):latest, which is why pullPolicy is Always there (IfNotPresent
# would silently keep serving whatever was last pulled under that mutable
# tag) - LOCAL's own image never leaves this machine, so IfNotPresent is
# what makes kind use the freshly loaded copy instead of trying to pull it.
LOCAL ?= 0
ifeq ($(LOCAL),1)
IMAGE_TAG   ?= dev
PULL_POLICY := IfNotPresent
HELM_DEPS   := kind-load
else
IMAGE_TAG   ?= latest
PULL_POLICY := Always
HELM_DEPS   :=
endif
IMAGE := $(IMAGE_REPO):$(IMAGE_TAG)

.PHONY: build test vet lint docker-build \
        kind-create kind-delete kind-load calico-install \
        helm-lint helm-template helm-install helm-install-no-auth helm-uninstall \
        demo-workloads demo-clean

build:
	cd golang && go build ./...

test:
	cd golang && go test ./... -race -cover

vet:
	cd golang && go vet ./...

lint:
	cd golang && golangci-lint run ./...

docker-build:
	docker build -t $(IMAGE) golang

# Uses kind/kind-config.yaml, which disables kind's default CNI (kindnet) -
# it doesn't enforce NetworkPolicy, so story 2 would appear to work (the API
# objects get created) without actually blocking any traffic. calico-install
# below installs a CNI that does enforce it.
kind-create:
	kind get clusters | grep -qx $(KIND_CLUSTER) || \
		kind create cluster --name $(KIND_CLUSTER) --config kind/kind-config.yaml

kind-delete:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

# CRDs are applied directly first: `helm install` in one shot occasionally
# tries to create the Installation custom resource before the API server has
# finished registering the CRD it depends on, which then fails once and
# succeeds on retry. Waiting for condition=Established closes that race
# properly instead of just relying on the delay between these two commands.
calico-install:
	helm repo add projectcalico https://docs.tigera.io/calico/charts >/dev/null 2>&1 || true
	helm repo update projectcalico
	helm show crds projectcalico/tigera-operator --version $(CALICO_VERSION) \
		| kubectl --context $(KIND_CONTEXT) apply -f - -o name \
		| xargs kubectl --context $(KIND_CONTEXT) wait --for=condition=Established --timeout=60s
	helm upgrade --install calico projectcalico/tigera-operator \
		--kube-context $(KIND_CONTEXT) --namespace tigera-operator --create-namespace \
		--version $(CALICO_VERSION)
	kubectl --context $(KIND_CONTEXT) wait --for=condition=Ready node --all --timeout=180s
	kubectl --context $(KIND_CONTEXT) -n calico-system wait --for=condition=Ready pods --all --timeout=180s

helm-lint:
	helm lint $(CHART)

helm-template:
	helm template $(RELEASE) $(CHART)

# Deferred (=) rather than immediate (:=) so it picks up PULL_POLICY /
# IMAGE_TAG / IMAGE_REPO as set by the LOCAL branch above, evaluated when a
# recipe actually uses it rather than when this line is read.
HELM_IMAGE_SET = --set image.repository=$(IMAGE_REPO) \
	--set image.tag=$(IMAGE_TAG) \
	--set image.pullPolicy=$(PULL_POLICY)

helm-install: $(HELM_DEPS)
	helm upgrade --install $(RELEASE) $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--kube-context $(KIND_CONTEXT) \
		$(HELM_IMAGE_SET)

# Same as helm-install but with the API's auth gate (TokenReview/
# SubjectAccessReview) disabled - see chart/values.yaml's auth.enabled
# comment. Local testing/demos against a cluster where minting caller
# tokens is inconvenient only, never a real deployment.
helm-install-no-auth: $(HELM_DEPS)
	helm upgrade --install $(RELEASE) $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--kube-context $(KIND_CONTEXT) \
		$(HELM_IMAGE_SET) \
		--set auth.enabled=false

helm-uninstall:
	helm uninstall $(RELEASE) --namespace $(NAMESPACE) --kube-context $(KIND_CONTEXT)

# Two workloads to isolate from each other (story 2) plus one deliberately
# under-replicated Deployment (story 1), matching the README's walkthrough.
demo-workloads:
	kubectl --context $(KIND_CONTEXT) create namespace ns-a --dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -
	kubectl --context $(KIND_CONTEXT) create namespace ns-b --dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -
	kubectl --context $(KIND_CONTEXT) -n ns-a create deployment frontend --image=nginx:alpine --replicas=1 --dry-run=client -o yaml \
		| kubectl --context $(KIND_CONTEXT) -n ns-a label --local -f - app=frontend -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -
	kubectl --context $(KIND_CONTEXT) -n ns-b create deployment backend --image=nginx:alpine --replicas=1 --dry-run=client -o yaml \
		| kubectl --context $(KIND_CONTEXT) -n ns-b label --local -f - app=backend -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -
	kubectl --context $(KIND_CONTEXT) -n ns-b expose deployment backend --port=80
	kubectl --context $(KIND_CONTEXT) -n default create deployment broken --image=nginx:this-tag-does-not-exist --replicas=3 --dry-run=client -o yaml \
		| kubectl --context $(KIND_CONTEXT) apply -f -
	kubectl --context $(KIND_CONTEXT) -n ns-a wait --for=condition=available deployment/frontend --timeout=90s
	kubectl --context $(KIND_CONTEXT) -n ns-b wait --for=condition=available deployment/backend --timeout=90s

demo-clean:
	kubectl --context $(KIND_CONTEXT) delete namespace ns-a ns-b --ignore-not-found
	kubectl --context $(KIND_CONTEXT) -n default delete deployment broken --ignore-not-found
