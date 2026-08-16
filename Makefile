IMAGE_REPO    ?= tyk-sre-assignment
IMAGE_TAG     ?= dev
IMAGE         := $(IMAGE_REPO):$(IMAGE_TAG)
KIND_CLUSTER  ?= tyk-sre-assignment
KIND_CONTEXT  := kind-$(KIND_CLUSTER)
CHART         := chart
RELEASE       ?= tyk-sre-assignment
NAMESPACE     ?= default
CALICO_VERSION ?= v3.29.1

.PHONY: build test vet lint docker-build \
        kind-create kind-delete kind-load calico-install \
        helm-lint helm-template helm-install helm-uninstall \
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
# succeeds on retry - installing the CRDs up front avoids that race.
calico-install:
	helm repo add projectcalico https://docs.tigera.io/calico/charts >/dev/null 2>&1 || true
	helm repo update projectcalico
	helm show crds projectcalico/tigera-operator --version $(CALICO_VERSION) | kubectl --context $(KIND_CONTEXT) apply -f -
	helm upgrade --install calico projectcalico/tigera-operator \
		--kube-context $(KIND_CONTEXT) --namespace tigera-operator --create-namespace \
		--version $(CALICO_VERSION)
	kubectl --context $(KIND_CONTEXT) wait --for=condition=Ready node --all --timeout=180s
	kubectl --context $(KIND_CONTEXT) -n calico-system wait --for=condition=Ready pods --all --timeout=180s

helm-lint:
	helm lint $(CHART)

helm-template:
	helm template $(RELEASE) $(CHART)

helm-install: kind-load
	helm upgrade --install $(RELEASE) $(CHART) \
		--namespace $(NAMESPACE) --create-namespace \
		--kube-context $(KIND_CONTEXT) \
		--set image.repository=$(IMAGE_REPO) \
		--set image.tag=$(IMAGE_TAG) \
		--set image.pullPolicy=IfNotPresent

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
