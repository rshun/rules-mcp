.PHONY: test vet build

test:
	GOTOOLCHAIN=local GOPROXY=off go test ./...

vet:
	GOTOOLCHAIN=local GOPROXY=off go vet ./...

build:
	mkdir -p dist
	CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/rules-mcp-linux-amd64 ./cmd/rules-mcp
	CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/rules-mcp-linux-arm64 ./cmd/rules-mcp
	cd dist && sha256sum rules-mcp-linux-amd64 rules-mcp-linux-arm64 > SHA256SUMS
