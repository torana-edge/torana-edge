#!/bin/sh
# fixtures-for-pkg.sh — the per-package fixture mapping (see
# scripts/fixtures-for-pkg.go for the AST-based implementation).
#
# usage: fixtures-for-pkg.sh <internal/<pkg>>
set -eu
exec env GOWORK=off go run ./scripts/fixtures-for-pkg.go "$@"
