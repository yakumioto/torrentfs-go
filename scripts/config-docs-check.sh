#!/usr/bin/env bash
# Regenerate the documentation fragments of the configuration schema and fail
# when the files in the repository differ from them.
#
# The fragments are derived from the schema and from the comments of the
# configuration structure, so a hand edit to one of them is a change the
# generator does not know about. The check keeps the files honest.
set -Eeuo pipefail
IFS=$'\n\t'

readonly ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"

cd "$ROOT_DIR"

# The generator is a Go program; the variable holds the command and its
# arguments, so the shell does not treat the whole line as one word.
readonly DOC=(go run ./internal/config/cmd/config-doc)

fail() {
	printf 'config docs: %s\n' "$*" >&2
	exit 1
}

check() {
	local fragment="$1" target="$2"
	local generated
	generated="$(mktemp)"
	trap 'rm -f "$generated"' RETURN
	"${DOC[@]}" "$fragment" >"$generated"
	if ! diff -u "$target" "$generated" >/dev/null; then
		printf 'config docs: %s is out of date; regenerate it with:\n' "$target" >&2
		printf '  go run ./internal/config/cmd/config-doc %s > %s\n' "$fragment" "$target" >&2
		diff -u "$target" "$generated" >&2 || true
		exit 1
	fi
}

check example-toml torrentfs.example.toml
check image-overlay docker/torrentfs.toml

# The README holds a generated block. The block is checked by writing it into
# a copy of the README and comparing the copy with the original: a hand edit
# to the block shows up as a difference.
readonly README_COPY="$(mktemp)"
trap 'rm -f "$README_COPY"' EXIT
cp README.md "$README_COPY"
"${DOC[@]}" write-readme-reference "$README_COPY"
if ! diff -u README.md "$README_COPY" >/dev/null; then
	printf 'config docs: the generated reference of the README is out of date; regenerate it with:\n' >&2
	printf '  go run ./internal/config/cmd/config-doc write-readme-reference README.md\n' >&2
	diff -u README.md "$README_COPY" >&2 || true
	exit 1
fi

printf 'config docs: the generated configuration files match the schema\n'
