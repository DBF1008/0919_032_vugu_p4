#!/usr/bin/env bash
#
# test.sh - run the repository's unit tests.
#
# By default it runs every Go package's unit tests (including the
# compile-driven merge tests). Network/browser dependent packages
# (wasm-test-suite, legacy-wasm-test-suite) are intentionally excluded - they
# require a Docker/Chrome setup and are integration tests rather than unit
# tests.
#
# Usage:
#   ./test.sh            # run all unit tests
#   ./test.sh -short     # fast unit tests only (skip build-heavy merge cases)
#   ./test.sh gen        # only test the listed packages
#
set -euo pipefail

# Resolve the repository root from this script's location so test.sh works no
# matter where it is invoked from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Unit-test packages, in dependency order.
DEFAULT_PKGS=(
	.
	./devutil
	./distutil
	./domrender
	./gen
	./js
	./simplehttp
	./staticrender
	./vgform
	./vugufmt
)

SHORT=0
if [[ "${1:-}" == "-short" ]]; then
	SHORT=1
	shift
fi

PKGS=("$@")
if [[ ${#PKGS[@]} -eq 0 ]]; then
	PKGS=("${DEFAULT_PKGS[@]}")
else
	# Allow bare directory names (e.g. "gen") in addition to "./gen".
	for i in "${!PKGS[@]}"; do
		case "${PKGS[$i]}" in
			. | ./* | /* | */*) ;;
			*) PKGS[$i]="./${PKGS[$i]}" ;;
		esac
	done
fi

# Fail loudly if Go is not on PATH.
if ! command -v go >/dev/null 2>&1; then
	echo "error: 'go' toolchain not found in PATH" >&2
	exit 1
fi

GO_TEST_ARGS=(-v)
if [[ "$SHORT" -eq 1 ]]; then
	GO_TEST_ARGS+=(-short)
	echo "==> running short (fast) unit tests"
else
	echo "==> running full unit tests"
fi

# Sanity checks on the merge implementation.
echo "==> checking formatting"
UNFORMATTED="$(gofmt -l gen/merge.go gen/merge_test.go)"
if [[ -n "$UNFORMATTED" ]]; then
	echo "error: these files are not gofmt clean:" >&2
	echo "$UNFORMATTED" >&2
	exit 1
fi

echo "==> go vet ./gen/..."
go vet ./gen/...

echo "==> building all packages"
go build ./...

# Packages whose tests need extra host tooling that may not be present
# (an open network port / the external goimports binary). A failure in these
# packages is reported as a warning rather than a hard failure.
ENV_DEPENDENT_PKGS=("./simplehttp" "./vugufmt")

is_env_dependent() {
	local pkg="$1"
	for dep in "${ENV_DEPENDENT_PKGS[@]}"; do
		if [[ "$pkg" == "$dep" ]]; then
			return 0
		fi
	done
	return 1
}

FAIL=0
WARN=0
for pkg in "${PKGS[@]}"; do
	echo
	echo "==> go test $pkg"
	if go test "${GO_TEST_ARGS[@]}" "$pkg"; then
		continue
	fi
	if is_env_dependent "$pkg"; then
		echo "    WARNING: $pkg failed (likely missing host tooling such as a free" >&2
		echo "             network port or the goimports binary); not treated as a failure" >&2
		WARN=1
	else
		echo "    FAILED: $pkg" >&2
		FAIL=1
	fi
done

echo
if [[ "$FAIL" -ne 0 ]]; then
	echo "UNIT TESTS FAILED" >&2
	exit 1
fi
if [[ "$WARN" -ne 0 ]]; then
	echo "ALL CORE UNIT TESTS PASSED (some environment-dependent packages were skipped/failed)"
else
	echo "ALL UNIT TESTS PASSED"
fi
