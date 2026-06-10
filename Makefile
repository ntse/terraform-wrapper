BINDIR ?= bin
GO ?= go
VERSION ?= $(shell git rev-parse --short=8 HEAD)
LDFLAGS := -X 'terraform-wrapper/cmd/terraform-wrapper/commands.wrapperVersion=$(VERSION)'
GOBIN ?= /usr/local/bin/
OUTFILE ?= terraform-wrapper

all: install

build:
	@echo "Building terraform-wrapper for $(GOOS)/$(GOARCH) with version $(VERSION)"
	@mkdir -p $(BINDIR)

	$(GO) build -ldflags "$(LDFLAGS)"  -o $(BINDIR)/$(OUTFILE) ./cmd/terraform-wrapper

install:
	@echo "Installing terraform-wrapper for $(GOOS)/$(GOARCH) with version $(VERSION) in $(GOBIN)"
	
	GOBIN=$(GOBIN) $(GO) install -ldflags "$(LDFLAGS)" ./cmd/terraform-wrapper

test:
	$(GO) test ./...

test-unit:
	$(GO) test -count=1 -short ./internal/...

test-integration:
	$(GO) test -count=1 -run Integration ./internal/executor/...

fmt:
	$(GO) fmt ./...

clean:
	@echo "Removing .terraform directories and lock files"
	@find . -type d -name .terraform -prune -exec rm -rf {} +
	@find . -type f -name .terraform.lock.hcl -delete

.PHONY: all build install test test-unit test-integration fmt clean
