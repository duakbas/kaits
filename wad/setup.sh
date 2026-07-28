#!/usr/bin/env bash
# One-time setup for the wad daemon. Run from inside the wad/ folder.
#
# Prereqs (macOS):
#   brew install go
#   xcode-select --install    # gives you a C compiler; go-sqlite3 needs CGO
#
set -e

# go-sqlite3 is a CGO package — it needs a C compiler at build time. On macOS
# that comes from the Command Line Tools. If `cc` is missing, stop early with
# a clear message rather than a wall of linker errors.
if ! command -v cc >/dev/null 2>&1; then
  echo "!! No C compiler found. Run:  xcode-select --install"
  echo "!! (needed because the sqlite session store uses CGO)"
  exit 1
fi
export CGO_ENABLED=1

echo ">> adding whatsmeow at latest (writes the real version into go.mod)"
go get go.mau.fi/whatsmeow@latest

echo ">> resolving all deps + writing go.sum"
go mod tidy

echo ">> building"
go build ./...

echo ""
echo ">> OK. Build succeeded."
echo ">> Start it with:"
echo ">>     WAD_TOKEN=changeme go run ./cmd/wad"
echo ">> First run prints a QR — scan from WhatsApp > Linked devices."
