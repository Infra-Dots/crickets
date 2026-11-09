GOLANGCI_LINT_VERSION ?= v2.12.2
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
IMAGE ?= ghcr.io/infra-dots/crickets
TAG ?= dev

.PHONY: test lint build image
test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	go vet ./...
	$(GOLANGCI_LINT) run ./...

build:
	CGO_ENABLED=0 go build -o bin/crickets ./cmd/crickets

image:
	docker build -t $(IMAGE):$(TAG) .

E2E_KUBECONFIG ?= $(CURDIR)/.e2e/kubeconfig
.PHONY: e2e-up e2e e2e-down
e2e-up:
	KUBECONFIG=$(E2E_KUBECONFIG) IMAGE=$(IMAGE) hack/e2e-up.sh

e2e:
	KUBECONFIG=$(E2E_KUBECONFIG) go test -tags e2e ./test/e2e/ -v -count=1 -timeout 30m

e2e-down:
	hack/e2e-down.sh
