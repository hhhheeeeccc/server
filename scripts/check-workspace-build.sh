#!/bin/sh
# Verify the actual Go dependency graph, including a build without local flags.
set -eu

SERVER_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$SERVER_DIR"
unset GOFLAGS GOWORK

EXPECTED_DIR="$SERVER_DIR/workspace-enterprise"
MODULE_DIR=$(go list -mod=readonly -m -f '{{.Dir}}' github.com/mattermost/enterprise)
if [ "$MODULE_DIR" != "$EXPECTED_DIR" ]; then
    echo "Enterprise must resolve inside this checkout: $EXPECTED_DIR (got $MODULE_DIR)" >&2
    exit 1
fi

DEPENDENCIES=$(mktemp)
trap 'rm -f "$DEPENDENCIES"' EXIT HUP INT TERM
go list -mod=readonly -deps -f '{{.ImportPath}}' ./cmd/mattermost > "$DEPENDENCIES"
for PACKAGE in \
    github.com/mattermost/enterprise \
    github.com/mattermost/enterprise/compliance \
    github.com/mattermost/enterprise/channel_read_receipts \
    github.com/mattermost/enterprise/post_forwarding \
    github.com/mattermost/enterprise/visual_editor \
    github.com/mattermost/enterprise/autotranslation \
    github.com/mattermost/mattermost/server/v8/enterprise/metrics
do
    if ! grep -Fxq "$PACKAGE" "$DEPENDENCIES"; then
        echo "Default server build is missing $PACKAGE" >&2
        exit 1
    fi
done
echo 'Default server build includes the bundled Workspace Enterprise source.'
