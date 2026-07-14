# Image URL to use all building/pushing image targets
IMG ?= us-docker.pkg.dev/ai-on-gke/kuberay-tpu-webhook/tpu-webhook:v1.2.5-gke.1

# For europe, use europe-docker.pkg.dev/ai-on-gke/kuberay-tpu-webhook/tpu-webhook

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

all: webhook

# Build manager binary
webhook:
	go build -o bin/kuberay-tpu-webhook main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
run: webhook
	go run ./main.go

# Run formatting against code.
fmt:
	go fmt ./...
	@if command -v pre-commit >/dev/null 2>&1; then \
		pre-commit run --all-files; \
	else \
		echo "Warning: pre-commit not found. Skipping python formatting."; \
	fi

# Run go vet against code.
vet:
	go vet ./...

# Run go test against code.
test:
	go test -race -timeout 1m ./...

# Run E2E tests.
e2e:
	./scripts/run-e2e.sh

uninstall:
	kubectl delete -f deployments/

# Deploy the webhook in-cluster
deploy:
	kubectl apply -f deployments/

# Build the docker image
docker-build:
	docker build . -t ${IMG}

# Push the docker image
docker-push:
	docker push ${IMG}

deploy-cert:
	kubectl apply -f certs/

uninstall-cert:
	kubectl delete -f certs/

img-swap:
	docker build . -t ${IMG}
	docker push ${IMG}
	EDITOR="sed -i \"s|^\( \+\)image: .*$$|\1image: ${IMG}|\"" kubectl edit deployment -n ray-system kuberay-tpu-webhook

.PHONY: webhook run fmt vet test e2e deploy uninstall docker-build docker-push deploy-cert uninstall-cert img-swap
