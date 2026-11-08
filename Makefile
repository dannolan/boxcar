GO ?= go

.PHONY: build check test golden race cross fmt pyref clean

build:
	$(GO) build ./...

## check — everything CI runs on a normal push
check: build
	$(GO) vet ./...
	@out="$$(gofmt -l internal cmd)"; if [ -n "$$out" ]; then \
		echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	$(GO) test ./internal/... ./cmd/... -count=1

test:
	$(GO) test ./internal/... ./cmd/... -count=1

## golden — STRICT byte-exactness validation vs the Python reference.
## Missing reference env = hard failure (see docs/PYREF.md).
golden:
	NSPBOX_REQUIRE_PYREF=1 $(GO) test -run 'Cross|Golden' -count=1 \
		./internal/nsp/ ./internal/dlc/ ./internal/nup/

race:
	$(GO) test -race ./internal/nup/ ./internal/nsp/ ./internal/nuptools/ ./cmd/...

## cross — build all release targets into dist/ (no packaging)
cross:
	bash scripts/cross.sh

fmt:
	gofmt -w internal cmd

pyref:
	bash scripts/setup-pyref.sh

clean:
	rm -rf dist
