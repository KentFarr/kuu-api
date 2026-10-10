.PHONY: build test vet fmt fmt-check lint check clean demo demo-checkout demo-logs demo-down

# If your user isn't in the docker group: make demo DOCKER="sudo docker"
DOCKER ?= docker
DEMO_COMPOSE = $(DOCKER) compose -f docker-compose.demo.yml

CMDS := server migrate backup fakeptero demo-checkout

build:
	@mkdir -p bin
	@for cmd in $(CMDS); do \
		echo "go build -o bin/$$cmd ./cmd/$$cmd"; \
		go build -o bin/$$cmd ./cmd/$$cmd || exit 1; \
	done

test:
	go test -race -cover ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

lint:
	golangci-lint run

check: fmt-check vet test

clean:
	rm -rf bin/

# Local demo: fake panel, Mailpit and Postgres in Docker; no real accounts.
demo:
	$(DEMO_COMPOSE) up -d --build
	@echo "waiting for kuu-api..."
	@for i in $$(seq 1 60); do \
		curl -fsS http://localhost:8081/livez >/dev/null 2>&1 && break; \
		if [ $$i -eq 60 ]; then echo "kuu-api did not become ready; see: make demo-logs"; exit 1; fi; \
		sleep 1; \
	done
	@echo ""
	@echo "kuu-api is up:  http://localhost:8081/readyz"
	@echo "emails:         http://localhost:8025"
	@echo "metrics:        http://localhost:8081/metrics  (Authorization: Bearer demo-admin-token)"
	@echo ""
	@echo "Next: make demo-checkout     (or: make demo-checkout ARGS=\"-repeat\")"

# Send a signed checkout.session.completed. Flags via ARGS, e.g.
#   make demo-checkout ARGS="-game rust -plan 16GB -email you@example.com"
demo-checkout:
	go run ./cmd/demo-checkout $(ARGS)

demo-logs:
	$(DEMO_COMPOSE) logs -f kuu-api fakeptero

demo-down:
	$(DEMO_COMPOSE) down -v
