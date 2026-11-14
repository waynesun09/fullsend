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
#   BT_SUITE       behaviour (default; runs the features given) or playback
#                  (the @playback suite, -tags playback)
#   AGENTS_SEED    1 to mirror github.com/fullsend-ai/agents into the
#                  emulator as fullsend-ai/agents; the shipped agents (which
#                  playback runs) resolve from it. Default 1 for playback.
#   AGENTS_REF     agents ref to mirror as main (default main)
#   AGENTS_PRS     agents pull request numbers merged on top, space-separated
#                  (e.g. "1871 1873", the host-aware ops scripts)
# Phase timings go to $STATE_DIR/phases.tsv and the step summary.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
STATE="${STATE_DIR:-${RUNNER_TEMP:-/tmp}/bt-emulator}"
mkdir -p "$STATE"
FEATURES=("${@:-features/triage/triage.feature}")
SUITE="${BT_SUITE:-behaviour}"
case "$SUITE" in
  behaviour) AGENTS_SEED="${AGENTS_SEED:-0}" ;;
  playback) AGENTS_SEED="${AGENTS_SEED:-1}" ;;
  *) echo "BT_SUITE must be behaviour or playback, got $SUITE" >&2; exit 2 ;;
esac
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
api() { curl -sS -o "$STATE/api-body" -w '%{http_code}' -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' "$@"; }
# ensure_source_repo NAME: create public fullsend-ai/NAME, a source to fetch
# from only, so its own CI (lint, test, semgrep) is turned off and cannot
# start in the emulator and hold the runners. Fails loudly.
ensure_source_repo() {
  local code
  code=$(api -X POST "$EMU/api/v3/orgs/fullsend-ai/repos" -d "{\"name\":\"$1\",\"private\":false}")
  case "$code" in 201|422) ;; *) echo "::error::creating fullsend-ai/$1: HTTP $code $(head -c 300 "$STATE/api-body")"; return 1 ;; esac
  for _ in $(seq 1 20); do
    [ "$(api "$EMU/api/v3/repos/fullsend-ai/$1")" = 200 ] && break
    sleep 0.5
  done
  code=$(api -X PUT "$EMU/api/v3/repos/fullsend-ai/$1/actions/permissions" -d '{"enabled":false}')
  [ "$code" = 204 ] || { echo "::error::turning Actions off for fullsend-ai/$1: HTTP $code $(head -c 300 "$STATE/api-body")"; return 1; }
}
code=$(api -X POST "$EMU/api/v3/admin/organizations" -d '{"login":"fullsend-ai","admin":"admin"}')
case "$code" in 201|422) ;; *) echo "::error::creating org fullsend-ai: HTTP $code $(head -c 300 "$STATE/api-body")"; exit 1 ;; esac
ensure_source_repo fullsend
sha=$(git -C "$ROOT" rev-parse HEAD)
git -C "$ROOT" -c credential.helper= -c http.extraheader= push -q --force \
  "https://admin:$ADMIN@$EMU_HOST/fullsend-ai/fullsend.git" "$sha:refs/heads/main"
phase upstream-push

# 4b. The shipped agents as fullsend-ai/agents: a run without its own agent
# config falls back to fullsend-ai/agents@main on the configured forge.
if [ "$AGENTS_SEED" = 1 ]; then
  agents="$STATE/agents.git"
  rm -rf "$agents"
  git clone -q --no-checkout "https://github.com/fullsend-ai/agents.git" "$agents"
  git -C "$agents" checkout -q --detach "${AGENTS_REF:-main}"
  for pr in ${AGENTS_PRS:-}; do
    git -C "$agents" fetch -q origin "pull/$pr/head"
    git -C "$agents" -c user.name=bt-emulator -c user.email=bt-emulator@localhost \
      merge -q --no-edit -m "bt-emulator: merge agents#$pr" FETCH_HEAD
  done
  ensure_source_repo agents
  git -C "$agents" -c credential.helper= -c http.extraheader= push -q --force \
    "https://admin:$ADMIN@$EMU_HOST/fullsend-ai/agents.git" "HEAD:refs/heads/main"
  echo "fullsend-ai/agents main on the emulator: $(git -C "$agents" rev-parse --short HEAD) (${AGENTS_REF:-main}${AGENTS_PRS:+ + PRs $AGENTS_PRS})"
  phase agents-push
fi

# 5. The suite, emulator mode.
status=0
(
  cd "$ROOT"
  unset GITHUB_TOKEN GITHUB_ACTIONS
  export ENVIRONMENT=emulator BEHAVIOUR_GITHUB_URL="$EMU" GH_TOKEN="$E2E_TOKEN" \
    FULLSEND_MINT_URL="$MINT" BEHAVIOUR_APP_SET="$APP_SET" BEHAVIOUR_CAPABILITIES='' \
    BEHAVIOUR_POOL_SIZE="${BEHAVIOUR_POOL_SIZE:-1}" \
    GODOG_CONCURRENCY=1 BEHAVIOUR_ARTIFACT_DIR="$STATE/bt-artifacts" \
    BEHAVIOUR_RUN_POLL_INTERVAL="${BEHAVIOUR_RUN_POLL_INTERVAL:-3s}" \
    BEHAVIOUR_NEGATIVE_SETTLE="${BEHAVIOUR_NEGATIVE_SETTLE:-20s}"
  if [ "$SUITE" = playback ]; then
    go test -tags playback -count=1 -timeout 40m -v ./e2e/behaviour/ -run TestPlaybackSuite
  else
    BEHAVIOUR_FEATURES="${FEATURES[*]}" \
      go test -tags behaviour -count=1 -timeout 40m -v ./e2e/behaviour/ -run TestBehaviourSuite
  fi
) 2>&1 | tee "$STATE/bt-run.log" || status=$?
phase suite
exit "$status"
