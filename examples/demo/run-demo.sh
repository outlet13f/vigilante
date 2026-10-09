#!/usr/bin/env bash
# End-to-end demo of automatic rollback and the safety circuit breaker.
# Run from the repository root:  ./examples/demo/run-demo.sh
# Needs: go, curl. Works on Linux, macOS and Git Bash on Windows.
set -u
cd "$(dirname "$0")/../.."

RUN=examples/demo/run
CFG=examples/demo/vigilante.yaml
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;; esac
VIG="bin/vigilante$EXE"
APP_URL="http://127.0.0.1:18080"
export DEMO_ROLLBACK_HOST="127.0.0.1:18080"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
expect() { # expect <wanted-exit> <got-exit> <label>
  if [ "$1" = "$2" ]; then printf '\033[1;32mOK\033[0m   %s (exit %s)\n' "$3" "$2"
  else printf '\033[1;31mFAIL\033[0m %s (exit %s, wanted %s)\n' "$3" "$2" "$1"; FAILED=1; fi
}
FAILED=0

say "build"
mkdir -p bin && go build -o "$VIG" ./cmd/vigilante && go build -o "bin/fakeapp$EXE" ./cmd/fakeapp || exit 1
rm -rf "$RUN" && mkdir -p "$RUN"

say "start fakeapp v1 (healthy; versions v2 are bad)"
"bin/fakeapp$EXE" -version v1 -bad v2 -access-log "$RUN/access.log" -app-log "$RUN/app.log" >"$RUN/fakeapp.out" 2>&1 &
APP_PID=$!
trap 'kill $APP_PID 2>/dev/null' EXIT
for _ in $(seq 1 50); do curl -fs "$APP_URL/health" >/dev/null && break; sleep 0.2; done

say "1. capture pre-deploy baseline (10s)"
"$VIG" baseline -c "$CFG" --service demo --window 10s --out "$RUN/baseline.json" >/dev/null 2>"$RUN/baseline.log"
cat "$RUN/baseline.json"

say "2. deploy BAD v2 -> canary watch should detect it and roll back to v1 (exit 2)"
curl -fs -X POST "$APP_URL/admin/deploy?version=v2"
"$VIG" watch -c "$CFG" --id demo-bad-1 --service demo --version v2 --previous v1 --phase canary \
  --baseline "$RUN/baseline.json" >"$RUN/watch1.json" 2>"$RUN/watch1.log"
CODE=$?
grep -E 'deployment state|step|verdict' "$RUN/watch1.log" | sed 's/^/   /' | cut -c1-220
expect 2 "$CODE" "bad release rolled back automatically"
printf '     running version now: %s' "$(curl -fs "$APP_URL/admin/version")"

say "3. deploy BAD v2 again, but the rollback path is broken -> rollback fails, circuit opens (exit 3)"
curl -fs -X POST "$APP_URL/admin/deploy?version=v2"
DEMO_ROLLBACK_HOST=127.0.0.1:18099 "$VIG" watch -c "$CFG" --id demo-bad-2 --service demo --version v2 --previous v1 \
  --phase canary --baseline "$RUN/baseline.json" >"$RUN/watch2.json" 2>"$RUN/watch2.log"
expect 3 "$?" "failed rollback escalates to a human"
"$VIG" circuit -c "$CFG" status 2>/dev/null | sed 's/^/   /'

say "4. while the circuit is OPEN, the deployment gate is closed (exit 3)"
"$VIG" watch -c "$CFG" --id demo-next --service demo --version v3 --previous v1 --phase canary >/dev/null 2>"$RUN/watch3.log"
expect 3 "$?" "new deployments are refused"
tail -1 "$RUN/watch3.log" | cut -c1-200 | sed 's/^/   /'

say "5. operator fixes things by hand, then resets the circuit"
curl -fs -X POST "$APP_URL/admin/deploy?version=v1"
"$VIG" circuit -c "$CFG" reset >/dev/null 2>&1
"$VIG" circuit -c "$CFG" status 2>/dev/null | grep '"state"' | sed 's/^/   /'
echo "   (waiting 11s so the 10s error-rate window no longer contains v2's errors)"
sleep 11

say "6. deploy GOOD v1.1 -> canary passes after the 20s window (exit 0)"
curl -fs -X POST "$APP_URL/admin/deploy?version=v1.1"
"$VIG" watch -c "$CFG" --id demo-good --service demo --version v1.1 --previous v1 --phase canary \
  --baseline "$RUN/baseline.json" >"$RUN/watch4.json" 2>"$RUN/watch4.log"
expect 0 "$?" "healthy release promoted"

say "journal (audit trail) — $RUN/journal.jsonl"
"$VIG" status -c "$CFG" 2>/dev/null | sed 's/^/   /'
exit $FAILED
