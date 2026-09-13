.PHONY: dev build run test clean

# Two-process development: Vite serves the client and proxies /api to Go.
dev:
	@echo "Run in two terminals:"
	@echo "  make run-api"
	@echo "  cd web && npm run dev   # http://localhost:5173"

run-api:
	go run ./cmd/server

# Build the client into cmd/server/dist, then embed it in the binary.
#
# Vite empties that directory on every build, which takes the committed
# placeholder with it. The placeholder has to exist for `go:embed all:dist` to
# compile on a fresh clone, before anyone has run a client build — so it is
# restored here rather than relied on to survive.
build:
	cd web && npm ci && npm run build
	touch cmd/server/dist/.gitkeep
	go build -o macscout ./cmd/server

run: build
	./macscout

test:
	go test ./...
	cd web && npx tsc -b

clean:
	rm -f macscout
	rm -rf cmd/server/dist/assets cmd/server/dist/index.html
	touch cmd/server/dist/.gitkeep
