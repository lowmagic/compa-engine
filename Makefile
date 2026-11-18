.PHONY: check fmt vet staticcheck lint test tidy standards sync-standards
SHELL := bash
ORG = https://github.com/lowmagic/.github

# staticcheck is usually installed with go install, which puts it in
# GOPATH/bin; that is often not on PATH.
STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || echo $$(go env GOPATH)/bin/staticcheck)

check: fmt vet staticcheck lint test tidy
	git ls-files -z '*.sh' | xargs -0 -r shellcheck

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

staticcheck:
	$(STATICCHECK) ./...

# lint fails only on issues in lines changed since the merge base with
# origin/main (.golangci.yml): the code from before is debt.
lint:
	golangci-lint run ./...

# The engine package runs for 8 to 9 minutes under -race on a CI runner or a
# laptop, and longer on Linux with bubblewrap running its commands, past go
# test's default 10-minute limit; the limit catches a hang, not slowness.
test:
	go test -race -timeout 30m ./...

tidy:
	go mod tidy -diff

standards:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	git clone -q --depth 1 --branch main $(ORG) "$$tmp" && \
	"$$tmp/code-standards/sync.sh" check "$$tmp"

sync-standards:
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	git clone -q --depth 1 --branch main $(ORG) "$$tmp" && \
	"$$tmp/code-standards/sync.sh" sync "$$tmp" "$$(git -C "$$tmp" rev-parse HEAD)"
