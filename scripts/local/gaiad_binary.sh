# Resolve the Gaia binary for local E2E scripts. Source this after cd'ing to
# the repository root.
#
# Priority:
#   1. GAIAD=/path/to/gaiad
#   2. GAIA_DIR/build/gaiad, default GAIA_DIR=../gaia
#
# Fail loud if Gaia is missing, is not on the custom IBC host branch, or has not
# been built. Falling back to an older PATH binary here produces a chain that
# does not match the relayer/L2 E2E assumptions.

GAIAD_BINARY_SOURCED=0
(return 0 2>/dev/null) && GAIAD_BINARY_SOURCED=1

gaiad_fail() {
    printf '[gaiad_binary] ERROR: %s\n' "$*" >&2
    if [ "$GAIAD_BINARY_SOURCED" -eq 1 ]; then
        return 1
    fi
    exit 1
}

GAIA_DIR=${GAIA_DIR:-../gaia}
GAIA_REQUIRED_BRANCH=${GAIA_REQUIRED_BRANCH:-test/ibc-host-customs}

if [ -n "${GAIAD:-}" ]; then
    case "$GAIAD" in
        */*)
            [ -x "$GAIAD" ] || { gaiad_fail "GAIAD is not executable: $GAIAD"; return 1; }
            ;;
        *)
            GAIAD_RESOLVED=$(command -v "$GAIAD" 2>/dev/null || true)
            [ -n "$GAIAD_RESOLVED" ] || { gaiad_fail "GAIAD command not found: $GAIAD"; return 1; }
            GAIAD=$GAIAD_RESOLVED
            ;;
    esac
else
    [ -d "$GAIA_DIR" ] ||
        { gaiad_fail "Gaia checkout not found at $GAIA_DIR; set GAIAD=/path/to/gaiad or GAIA_DIR=/path/to/gaia"; return 1; }

    GAIA_DIR=$(cd "$GAIA_DIR" && pwd)
    if [ -n "$GAIA_REQUIRED_BRANCH" ]; then
        GAIA_BRANCH=$(git -C "$GAIA_DIR" branch --show-current 2>/dev/null || true)
        [ "$GAIA_BRANCH" = "$GAIA_REQUIRED_BRANCH" ] ||
            { gaiad_fail "$GAIA_DIR is on branch ${GAIA_BRANCH:-unknown}; expected $GAIA_REQUIRED_BRANCH"; return 1; }
    fi
    GAIAD="$GAIA_DIR/build/gaiad"
    [ -x "$GAIAD" ] ||
        { gaiad_fail "$GAIAD is missing; build it with: cd $GAIA_DIR && GOTOOLCHAIN=go1.25.7 make build"; return 1; }
fi

export GAIAD
printf '[gaiad_binary] using %s\n' "$GAIAD"
