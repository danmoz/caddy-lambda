#!/bin/sh

set -eu

go_files=$(find . -name '*.go' -type f -print)

if [ "${1:-}" = "--check" ]; then
	test -z "$(gofmt -l $go_files)"
	exit 0
fi

gofmt -w $go_files
