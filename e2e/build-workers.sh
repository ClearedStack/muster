#!/bin/sh
# Run it with sh (the file is committed without the executable bit).
# Builds what the end-to-end harness runs: the eight worker binaries named in
# workers.txt, Muster from this checkout, and Logbook's migrations.
#
#   E2E_BIN_DIR              where the binaries go (default: e2e/bin)
#   E2E_PLAINFORGE_BASE      where to clone from (default: http://127.0.0.1:13000/ClearedStack)
#
# Nothing here is pushed anywhere. Credentials are not needed: the repositories are read over HTTP.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
bin=${E2E_BIN_DIR:-$here/bin}
base=${E2E_PLAINFORGE_BASE:-http://127.0.0.1:13000/ClearedStack}

mkdir -p "$bin"
bin=$(cd "$bin" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/e2e-build.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
: > "$bin/versions.txt"

(cd "$root" && go build -o "$bin/muster" ./cmd/muster)
echo "muster $(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown) (this checkout)" >> "$bin/versions.txt"

grep -v '^[[:space:]]*\(#\|$\)' "$here/workers.txt" | while read -r repo branch; do
	git clone -q --depth 1 --branch "$branch" "$base/$repo.git" "$work/$repo"
	sha=$(git -C "$work/$repo" rev-parse --short HEAD)
	if [ "$repo" = logbook ]; then
		rm -rf "$bin/logbook"
		mkdir -p "$bin/logbook"
		cp -R "$work/$repo/migrations" "$bin/logbook/migrations"
	else
		(cd "$work/$repo" && go build -o "$bin/$repo" "./cmd/$repo")
	fi
	echo "$repo $branch $sha" >> "$bin/versions.txt"
	echo "built $repo $branch $sha"
done
echo "binaries and logbook/migrations are in $bin"
