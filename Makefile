BINARY := k8s-monitor
PKG := github.com/rodrigos0ares/k8s-monitor

.PHONY: build run clean install test lint benchmark purge-pods

build:
	CGO_ENABLED=1 go build -o bin/$(BINARY) ./cmd/

run: build
	./bin/$(BINARY) start

install: build
	cp bin/$(BINARY) $(GOPATH)/bin/ 2>/dev/null || cp bin/$(BINARY) ~/go/bin/

clean:
	rm -rf bin/

test:
	go test ./...

lint:
	golangci-lint run ./...

benchmark:
	./bin/$(BINARY) benchmark --profile balanced --output ./benchmarks/latest.json --save-best qwen3.5-9b qwen3.5-9b-q5 phi3.5

# Delete stored logs for pods that no longer exist in the cluster.
# Pass DRY_RUN=1 to preview without deleting.
purge-pods: build
	./bin/$(BINARY) purge pods $(if $(DRY_RUN),--dry-run,)
