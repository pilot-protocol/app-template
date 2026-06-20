# Developer entrypoints. `make ci` mirrors the GitHub `ci` workflow, so what
# passes locally passes in CI.
.PHONY: ci build test race e2e e2e-managed fmt fmt-check vet lint cover docker-broker clean

ci: fmt-check vet build race e2e ## everything CI runs

build: ## build all binaries
	go build ./...

test: ## run unit tests
	go test ./...

race: ## run unit tests with the race detector + coverage
	go test -race -coverprofile=cover.out -covermode=atomic ./...

e2e: ## real-process broker end-to-end (multi-user, no external services)
	./scripts/e2e-broker.sh

e2e-managed: ## full publish→build→register→sign→broker→partner→meter+ratelimit e2e
	./scripts/e2e-managed.sh

fmt: ## format the code
	gofmt -w cmd internal

fmt-check: ## fail if anything needs formatting
	@u="$$(gofmt -l cmd internal)"; if [ -n "$$u" ]; then echo "needs gofmt:"; echo "$$u"; exit 1; fi

vet: ## go vet
	go vet ./...

lint: ## staticcheck (same as the CI lint job)
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

cover: race ## show per-package coverage
	@go tool cover -func=cover.out | tail -1

docker-broker: ## build the broker image
	docker build -f deploy/docker/broker.Dockerfile -t pilot-broker .

clean:
	rm -f cover.out


broker-tls: ## (on the VM) nginx + Lets Encrypt HTTPS for the broker
	sudo BROKER_HOST=broker.pilotprotocol.network bash deploy/setup-broker-tls.sh
