#!/bin/sh
# Runs the third-phase acceptance test (go-mapping §5 step 7e).
#
# Needs Linux with /dev/fuse, and Go. Inside a container:
#
#   docker run --rm --device /dev/fuse --cap-add SYS_ADMIN \
#     -v "$PWD":/src -w /src golang:1.26 sh scripts/acceptance.sh
#
# It builds the binary, starts it over MCP stdio, and walks the eight steps of
# the scenario. Any failed step fails the run.

set -eu

cd "$(dirname "$0")/.."

if [ "$(uname -s)" != "Linux" ]; then
	echo "acceptance: needs Linux for FUSE and process groups" >&2
	exit 1
fi
if [ ! -e /dev/fuse ]; then
	echo "acceptance: /dev/fuse is missing; pass --device /dev/fuse" >&2
	exit 1
fi

exec go test -count=1 -v -run TestThirdPhaseAcceptance ./cmd/agent-design/
