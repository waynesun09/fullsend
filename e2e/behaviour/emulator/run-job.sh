#!/usr/bin/env bash
# Run behaviour features against a GitHub emulator inside one Linux VM:
# github-emulator + the real actions/runner, fullsend's standalone mint, and
# this checkout pushed into the emulator as fullsend-ai/fullsend. Needs no
# repository secret: every credential is made for this run and dies with it.
#
#   e2e/behaviour/emulator/run-job.sh [feature ...]
#
# Settings (env):
#   EMULATOR_DIR   github-emulator checkout (scripts/ci-stack.sh)  [required]
#   EMULATOR_PY    python with its requirements.txt installed       [required]
#   STATE_DIR      logs and state (default $RUNNER_TEMP/bt-emulator)
#   UPSTREAM_TOKEN optional token the emulator uses to fetch github.com actions
# Phase timings go to $STATE_DIR/phases.tsv and the step summary.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
STATE="${STATE_DIR:-${RUNNER_TEMP:-/tmp}/bt-emulator}"
mkdir -p "$STATE"
FEATURES=("${@:-features/triage/triage.feature}")
EMU_HOST=ghemu.localhost
EMU="https://$EMU_HOST"
ORG=halfsend
APP_SET=fullsend-test
MINT_PORT=18502
MINT="http://127.0.0.1:$MINT_PORT"
ADMIN="ghp_run$(openssl rand -hex 16)"
PHASES="$STATE/phases.tsv"
: >"$PHASES"

phase_start=$(date +%s)
phase() { # phase NAME: close the previous phase and record its wall time
  local now; now=$(date +%s)
  printf '%s\t%s\n' "$1" "$((now - phase_start))" >>"$PHASES"
  echo "::notice::phase $1 took $((now - phase_start))s"
  phase_start=$now
}

# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap
stop_all() {
  if [ -f "$STATE/mint.pid" ]; then kill "$(cat "$STATE/mint.pid")" 2>/dev/null || true; fi
  (cd "$EMULATOR_DIR" && CI_STACK_DIR="$STATE/stack" scripts/ci-stack.sh stop) >/dev/null 2>&1 || true
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    { echo "| phase | seconds |"; echo "|---|---|"; awk -F'\t' '{print "| " $1 " | " $2 " |"}' "$PHASES"; } >>"$GITHUB_STEP_SUMMARY"
  fi
}
trap stop_all EXIT

# 1. Emulator + one real actions/runner over TLS on ghemu.localhost:443.
sudo sysctl -qw net.ipv4.ip_unprivileged_port_start=80
(
  cd "$EMULATOR_DIR"
  CI_STACK_DIR="$STATE/stack" CI_STACK_PYTHON="$EMULATOR_PY" \
  CI_STACK_RUNNER_KIND=upstream CI_STACK_RUNNERS="${EMULATOR_RUNNERS:-2}" \
  CI_STACK_TLS=1 CI_STACK_HOST="$EMU_HOST" CI_STACK_PORT=443 CI_STACK_SYSTEM_TRUST=1 \
  CI_STACK_LOCALHOST_DNS=1 CI_STACK_BIND=0.0.0.0 \
  GITHUB_EMULATOR_DEFAULT_ADMIN_TOKEN="$ADMIN" \
  GITHUB_EMULATOR_ACTIONS_UPSTREAM_TOKEN="${UPSTREAM_TOKEN:-}" \
    scripts/ci-stack.sh start
)
export SSL_CERT_FILE="$STATE/stack/tls/bundle.pem" GIT_SSL_CAINFO="$STATE/stack/tls/bundle.pem"
export CURL_CA_BUNDLE="$STATE/stack/tls/ca.pem" NODE_EXTRA_CA_CERTS="$STATE/stack/tls/ca.pem"
phase stack

# 2. One App per mint role plus the harness's e2e App, keys made by the emulator.
mkdir -p "$STATE/pem"
APP_SET=$APP_SET "$EMULATOR_PY" -I "$EMULATOR_DIR/scripts/ci-seed-mint-apps.py" "$EMU" "$ADMIN" "$ORG" "$STATE/pem" >"$STATE/role_app_ids.json"
E2E_TOKEN="$(APP_SET=$APP_SET "$EMULATOR_PY" -I "$EMULATOR_DIR/scripts/ci-seed-mint-apps.py" token "$EMU" "$ADMIN" "$ORG" e2e)"
echo "::add-mask::$E2E_TOKEN"
phase seed

# 3. fullsend's standalone mint, trusting the emulator's OIDC issuer and API.
(cd "$ROOT/cmd/mint" && go build -o "$STATE/mint" .)
hosts=""
for i in $(seq -f %02g 1 "${BEHAVIOUR_POOL_SIZE:-1}"); do hosts="$hosts,$ORG/test-repo-$i,$ORG/test-repo-$i-mint-host"; done
PORT=$MINT_PORT MINT_OIDC_ISSUER_URL="$EMU" MINT_GITHUB_API_URL="$EMU/api/v3" \
PEM_DIR="$STATE/pem" ROLE_APP_IDS="$(cat "$STATE/role_app_ids.json")" \
PER_REPO_WIF_REPOS='*' ALLOWED_WORKFLOW_FILES='*' WORKFLOW_HOST_REPOS="fullsend-ai/fullsend$hosts" \
  nohup "$STATE/mint" >"$STATE/mint.log" 2>&1 &
echo $! >"$STATE/mint.pid"
for _ in $(seq 1 50); do curl -s -o /dev/null "$MINT/" && break; sleep 0.2; done
phase mint

# 4. This checkout as fullsend-ai/fullsend, so the shim pinned to HEAD resolves.
api() { curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' "$@"; }
api -X POST "$EMU/api/v3/admin/organizations" -d '{"login":"fullsend-ai","admin":"admin"}' >/dev/null
api -X POST "$EMU/api/v3/orgs/fullsend-ai/repos" -d '{"name":"fullsend","private":false}' >/dev/null
# Only a source to fetch from: its own CI (lint, test, semgrep) must not
# start in the emulator and hold the runners.
api -X PUT "$EMU/api/v3/repos/fullsend-ai/fullsend/actions/permissions" -d '{"enabled":false}' >/dev/null
sha=$(git -C "$ROOT" rev-parse HEAD)
git -C "$ROOT" -c credential.helper= -c http.extraheader= push -q --force \
  "https://admin:$ADMIN@$EMU_HOST/fullsend-ai/fullsend.git" "$sha:refs/heads/main"
phase upstream-push

# 5. The behaviour suite, emulator mode.
status=0
(
  cd "$ROOT"
  unset GITHUB_TOKEN GITHUB_ACTIONS
  ENVIRONMENT=emulator BEHAVIOUR_GITHUB_URL="$EMU" GH_TOKEN="$E2E_TOKEN" \
  FULLSEND_MINT_URL="$MINT" BEHAVIOUR_APP_SET="$APP_SET" BEHAVIOUR_CAPABILITIES='' \
  BEHAVIOUR_FEATURES="${FEATURES[*]}" BEHAVIOUR_POOL_SIZE="${BEHAVIOUR_POOL_SIZE:-1}" \
  GODOG_CONCURRENCY=1 BEHAVIOUR_ARTIFACT_DIR="$STATE/bt-artifacts" \
  BEHAVIOUR_RUN_POLL_INTERVAL="${BEHAVIOUR_RUN_POLL_INTERVAL:-3s}" \
  BEHAVIOUR_NEGATIVE_SETTLE="${BEHAVIOUR_NEGATIVE_SETTLE:-20s}" \
    go test -tags behaviour -count=1 -timeout 40m -v ./e2e/behaviour/ -run TestBehaviourSuite
) 2>&1 | tee "$STATE/bt-run.log" || status=$?
phase suite
exit "$status"
