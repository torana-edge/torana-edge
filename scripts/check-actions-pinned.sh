#!/bin/sh
set -eu

# Mutable action tags let an upstream repository change executable CI code
# without a Torana review. Local and docker actions have separate trust roots;
# every repository action must use an immutable 40-hex commit.
failed=0
for workflow in .github/workflows/*.yml .github/workflows/*.yaml; do
	[ -f "$workflow" ] || continue
	while IFS= read -r line; do
		target=$(printf '%s\n' "$line" | sed -n 's/^[[:space:]-]*uses:[[:space:]]*\([^[:space:]#]*\).*/\1/p')
		[ -n "$target" ] || continue
		case "$target" in
			./* | docker://*) continue ;;
		esac
		case "$target" in
			*@*) ref=${target##*@} ;;
			*)
				echo "$workflow: action has no ref: $target" >&2
				failed=1
				continue
				;;
		esac
		if ! printf '%s\n' "$ref" | grep -Eq '^[0-9a-fA-F]{40}$'; then
			echo "$workflow: action ref is mutable: $target" >&2
			failed=1
		fi
	done <"$workflow"
done
exit "$failed"
