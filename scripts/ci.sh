#!/usr/bin/env bash
set -euo pipefail

# Linked worktrees live below this persistent clone but are not repository content.
unformatted="$(gofmt -l . | grep -Ev '^(\./)?\.worktrees/' || true)"
if [[ -n "${unformatted}" ]]; then
  printf 'The following Go files are not gofmt-formatted:\n%s\n' "${unformatted}" >&2
  exit 1
fi

go vet ./...
# The Go race detector requires cgo on supported Unix platforms.
export CGO_ENABLED=1
go test ./... -race -shuffle=on
(
  cd web
  npm ci
  npm run build
)
