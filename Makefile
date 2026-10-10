.PHONY: build test vet fmt fmt-check lint check clean

CMDS := server migrate backup

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
