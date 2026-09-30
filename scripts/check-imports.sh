#!/bin/sh
# Enforces the hexagonal import rules (Go itself doesn't):
#   internal/core imports nothing from the application,
#   an adapter under internal/ imports only the core.
# Only main.go (the composition root) sees concrete adapters.
set -eu
cd "$(dirname "$0")/.."
mod=$(go list -m)
edges=$(go list -f '{{$p := .ImportPath}}{{range .Imports}}{{$p}} {{.}}
{{end}}{{range .TestImports}}{{$p}} {{.}}
{{end}}' ./internal/...)
fail=0
while read -r pkg imp; do
	case "$imp" in "$mod"/*) ;; *) continue ;; esac
	if [ "$pkg" = "$mod/internal/core" ]; then
		echo "$pkg: core imports $imp"
		fail=1
	elif [ "$imp" != "$mod/internal/core" ]; then
		echo "$pkg: adapter imports $imp"
		fail=1
	fi
done <<EDGES
$edges
EDGES
exit $fail
