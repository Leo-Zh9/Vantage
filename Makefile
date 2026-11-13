BINARY  ?= bin/vantage
BACKEND ?= http://localhost:8000

.PHONY: build test bench vet fmt lint run loadgen docker clean

build:
	go build -o $(BINARY) ./cmd/vantage

test:
	go test ./...

bench:
	go test -run='^$$' -bench=. -benchmem ./internal/analytics/

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	golangci-lint run

# -ip-header X-Forwarded-For lets loadgen simulate many visitors locally; never
# use it where clients can reach Vantage directly.
run: build
	$(BINARY) -backend $(BACKEND) -ip-header X-Forwarded-For -ratelimit 100 -report-file report.md

loadgen:
	go run ./tools/loadgen -url http://localhost:8080 -n 100000 -visitors 5000

docker:
	docker build -t vantage:latest .

clean:
	rm -rf bin
