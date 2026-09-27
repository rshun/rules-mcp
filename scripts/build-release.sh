#!/usr/bin/env bash
set -euo pipefail

version="${1:-dev}"
if [[ ! "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$ ]]; then
  printf '%s\n' 'Invalid release version' >&2
  exit 1
fi
cd "$(dirname "$0")/.."
export CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOOS=linux
mkdir -p dist
for arch in amd64 arm64; do
  GOARCH="$arch" go build -trimpath -ldflags="-s -w -X main.version=$version" \
    -o "dist/rules-mcp-linux-$arch" ./cmd/rules-mcp
  staging="dist/package-$arch"
  mkdir -p "$staging"
  install -m 0755 "dist/rules-mcp-linux-$arch" "$staging/rules-mcp"
  install -m 0644 README.md LICENSE config.example.json deploy/rules-mcp.service "$staging/"
  tar -czf "dist/rules-mcp_${version}_linux_${arch}.tar.gz" \
    -C "$staging" rules-mcp README.md LICENSE config.example.json rules-mcp.service
done
install -m 0644 LICENSE dist/LICENSE
(
  cd dist
  sha256sum rules-mcp-linux-amd64 rules-mcp-linux-arm64 \
    "rules-mcp_${version}_linux_amd64.tar.gz" \
    "rules-mcp_${version}_linux_arm64.tar.gz" LICENSE > SHA256SUMS
)
