GO ?= go

TFPLUGINDOCS_VERSION ?= v0.25.0
GOVULNCHECK_VERSION ?= v1.6.0

.PHONY: default build install test testacc vet vuln fmt fmtcheck docs

default: build

build:
	$(GO) build ./...

install:
	$(GO) install .

# Unit tests (client, mock API, resource CRUD logic) — no TF_ACC needed.
test:
	$(GO) test ./... -timeout 120s

# Acceptance tests via terraform-plugin-testing against the httptest mock.
# Requires a terraform CLI in PATH; no real FCS API is contacted.
testacc:
	TF_ACC=1 $(GO) test ./internal/provider/ -v -timeout 10m

vet:
	$(GO) vet ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

fmt:
	gofmt -w .

fmtcheck:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

# Regenerate the Terraform Registry docs from the provider schema + examples/.
# Pinned so the CI docs-drift check is reproducible. Needs a terraform CLI.
docs:
	$(GO) run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION) \
		generate --provider-name fcs --rendered-provider-name "FCS"
