#!/bin/sh
# Run all unit tests in the repository.
set -e
cd "$(dirname "$0")"
go test ./...
