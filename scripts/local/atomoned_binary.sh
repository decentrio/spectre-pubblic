# Resolve the atomoned binary for local E2E scripts. Source this after cd'ing to
# the repository root.
#
# AtomOne hosts the Ethereum light client as a native Go module (no 08-wasm), in the
# atomone checkout's branch e2e/module-spectre.
#
# Priority:
#   1. ATOMONED=/path/to/atomoned
#   2. ATOMONE_DIR/build/atomoned, default ATOMONE_DIR=../atomone
#
# Fail loud if the checkout is missing, is on another branch, or has not been
# built: an older atomoned on PATH has no spectre client or an older one, and the
# failure shows up many steps later as an unknown client type.

ATOMONED_BINARY_SOURCED=0
(return 0 2>/dev/null) && ATOMONED_BINARY_SOURCED=1

atomoned_fail() {
    printf '[atomoned_binary] ERROR: %s\n' "$*" >&2
    if [ "$ATOMONED_BINARY_SOURCED" -eq 1 ]; then
        return 1
    fi
    exit 1
}

ATOMONE_DIR=${ATOMONE_DIR:-../atomone}
ATOMONE_REQUIRED_BRANCH=${ATOMONE_REQUIRED_BRANCH:-e2e/module-spectre}

if [ -n "${ATOMONED:-}" ]; then
    case "$ATOMONED" in
        */*)
            [ -x "$ATOMONED" ] || { atomoned_fail "ATOMONED is not executable: $ATOMONED"; return 1; }
            ;;
        *)
            ATOMONED_RESOLVED=$(command -v "$ATOMONED" 2>/dev/null || true)
            [ -n "$ATOMONED_RESOLVED" ] || { atomoned_fail "ATOMONED command not found: $ATOMONED"; return 1; }
            ATOMONED=$ATOMONED_RESOLVED
            ;;
    esac
else
    [ -d "$ATOMONE_DIR" ] ||
        { atomoned_fail "AtomOne checkout not found at $ATOMONE_DIR; set ATOMONED=/path/to/atomoned or ATOMONE_DIR=/path/to/atomone"; return 1; }

    ATOMONE_DIR=$(cd "$ATOMONE_DIR" && pwd)
    if [ -n "$ATOMONE_REQUIRED_BRANCH" ]; then
        ATOMONE_BRANCH=$(git -C "$ATOMONE_DIR" branch --show-current 2>/dev/null || true)
        [ "$ATOMONE_BRANCH" = "$ATOMONE_REQUIRED_BRANCH" ] ||
            { atomoned_fail "$ATOMONE_DIR is on branch ${ATOMONE_BRANCH:-unknown}; expected $ATOMONE_REQUIRED_BRANCH"; return 1; }
    fi
    ATOMONED="$ATOMONE_DIR/build/atomoned"
    [ -x "$ATOMONED" ] ||
        { atomoned_fail "$ATOMONED is missing; build it with: ./scripts/local/build_atomoned.sh"; return 1; }
fi

export ATOMONED
printf '[atomoned_binary] using %s\n' "$ATOMONED"
