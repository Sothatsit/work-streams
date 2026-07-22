#!/bin/bash
# End-to-end smoke test for the CLI, HTTP API, and store together.
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
test_dir="$(mktemp -d)"
server_pid=""
test_secret="quack-test-secret"

cleanup() {
    if [[ -n "$server_pid" ]]; then
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    rm -rf "$test_dir"
}
trap cleanup EXIT

"$repo/scripts/build.sh" >/dev/null

WORK_STREAM_SECRET="$test_secret" \
    "$repo/bin/ws-server" --data "$test_dir" --port 0 \
    >"$test_dir/server.log" 2>&1 &
server_pid=$!

port=""
ready=false
for _ in $(seq 1 50); do
    if ! kill -0 "$server_pid" 2>/dev/null; then
        cat "$test_dir/server.log" >&2
        exit 1
    fi
    port="$(sed -nE \
        's/.*listening on .*:([0-9]+) .*/\1/p' \
        "$test_dir/server.log" | tail -1)"
    if [[ -n "$port" ]] && curl -fsS \
        -H 'Work-Stream-API-Version: 1' \
        -H "Authorization: Bearer $test_secret" \
        "http://localhost:$port/api/status" >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 0.1
done
if [[ "$ready" != true ]]; then
    cat "$test_dir/server.log" >&2
    exit 1
fi

export WORK_STREAM_ADDRESS="localhost"
export WORK_STREAM_PORT="$port"
export WORK_STREAM_SECRET="$test_secret"
ws() { "$repo/bin/ws" "$@"; }

failures=0
check() {
    local desc="$1" expected="$2" actual="$3"
    if [[ "$actual" == *"$expected"* ]]; then
        echo "PASS: $desc"
    else
        echo "FAIL: $desc"
        echo "  expected to contain: $expected"
        echo "  actual: $actual"
        failures=$((failures + 1))
    fi
}

check_status() {
    local desc="$1" expected="$2"
    shift 2
    local actual
    actual="$(curl -sS -o /dev/null -w '%{http_code}' "$@")"
    check "$desc" "$expected" "$actual"
}

check "version is offline" "ws 0.1.0" "$(ws --version)"
ws status >/dev/null

check_status "secret is required" "401" \
    -H 'Work-Stream-API-Version: 1' \
    "http://localhost:$port/api/status"
check_status "API version is required" "400" \
    -H "Authorization: Bearer $test_secret" \
    "http://localhost:$port/api/status"
check_status "old routes are absent" "404" \
    -H 'Work-Stream-API-Version: 1' \
    -H "Authorization: Bearer $test_secret" \
    "http://localhost:$port/entries"

ws add todo "Find a Stock Image of a Duck" \
    --project Duck-Pond >/dev/null # e1
ws add note "Reviewed the duck PR" \
    --meta pr=https://example.com/pr/7 --jira QUACK-1 >/dev/null # e2
ws add note "Handle [duck]? literally" >/dev/null # e3

check "GLOB reaches the server" "[e1]" \
    "$(ws search --subject '*stock*')"
check "GLOB is ASCII case-insensitive" "[e2]" \
    "$(ws search --jira 'quack-*')"
check "positional search escapes GLOB characters" "[e3]" \
    "$(ws search '[duck]?')"
check "metadata pair keeps key and value together" "No matching entries." \
    "$(ws search --meta 'pr=QUACK-*')"
check "negation reaches the server" "e1" \
    "$(ws search --no-type note --id-only)"

ws edit e1 --body "The Long Form Detail" >/dev/null
check "content searches subject or body" "[e1]" \
    "$(ws search --content '*long form*')"
check "entry shows the body" "The Long Form Detail" "$(ws entry e1)"
check "list hides the body" "0" \
    "$(ws search Stock | grep -c 'Long Form Detail' || true)"

echo
if [[ "$failures" -gt 0 ]]; then
    echo "$failures FAILURES"
    exit 1
fi
echo "All e2e tests passed."
