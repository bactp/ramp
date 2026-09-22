CONTROLLER_GEN ?= $(shell pwd)/bin/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.17.2

.PHONY: build
build: ## Build the RAMP binaries.
	go build -o bin/ramp-manager ./cmd/manager
	go build -o bin/rampctl ./cmd/rampctl

.PHONY: vet
vet:
	go vet ./...

.PHONY: controller-gen
controller-gen:
	@test -x $(CONTROLLER_GEN) || \
	  GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: manifests
manifests: controller-gen ## Regenerate deepcopy functions and CRDs.
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/ramp/crd

.PHONY: install
install: ## Install the RAMP CRDs on the management cluster.
	kubectl --kubeconfig $(HOME)/mgmt.kubeconfig apply -f config/ramp/crd/

.PHONY: run
run: build ## Run the manager out of cluster against mgmt.
	KUBECONFIG=$(HOME)/mgmt.kubeconfig ./bin/ramp-manager \
	  --cluster=workload01=$(HOME)/workload01.kubeconfig \
	  --cluster=workload02=$(HOME)/workload02.kubeconfig \
	  --artifact-store-endpoint=192.168.28.158:32000 \
	  --health-probe-bind-address=:8082
