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
#   BEHAVIOUR_CAPABILITIES  passed to the suite (default none). With
#                  inference-gateway in it, a test gateway runs on
#                  https://gw.ghemu.localhost (gateway-stub.sh) and the suite
#                  gets E2E_INFERENCE_GATEWAY_URL and _TEST_KEY.
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
CAPABILITIES="${BEHAVIOUR_CAPABILITIES:-}"
GATEWAY=0
case ",$CAPABILITIES," in *,inference-gateway,*) GATEWAY=1 ;; esac
GW_HOST="gw.$EMU_HOST"
GW_IP=10.254.0.1

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
  if [ "$GATEWAY" = 1 ]; then GW_HOST=$GW_HOST GW_IP=$GW_IP "$ROOT/e2e/behaviour/emulator/gateway-stub.sh" stop "$STATE/gateway" || true; fi
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
  CI_STACK_DNS_EXTRA="$([ "$GATEWAY" = 1 ] && echo "$GW_HOST=$GW_IP")" \
  GITHUB_EMULATOR_DEFAULT_ADMIN_TOKEN="$ADMIN" \
  GITHUB_EMULATOR_ACTIONS_UPSTREAM_TOKEN="${UPSTREAM_TOKEN:-}" \
    scripts/ci-stack.sh start
)
export SSL_CERT_FILE="$STATE/stack/tls/bundle.pem" GIT_SSL_CAINFO="$STATE/stack/tls/bundle.pem"
export CURL_CA_BUNDLE="$STATE/stack/tls/ca.pem" NODE_EXTRA_CA_CERTS="$STATE/stack/tls/ca.pem"
phase stack

# OpenShell's egress proxy (in the sandbox supervisor) checks upstream TLS
# against the supervisor image's CA bundle, so trust the stack CA there.
"$ROOT/e2e/behaviour/emulator/supervisor-ca-image.sh" "$STATE/stack/tls/ca.pem"
phase supervisor-ca

# 2. One App per mint role plus the harness's e2e App, keys made by the emulator.
mkdir -p "$STATE/pem"
APP_SET=$APP_SET "$EMULATOR_PY" -I "$EMULATOR_DIR/scripts/ci-seed-mint-apps.py" "$EMU" "$ADMIN" "$ORG" "$STATE/pem" >"$STATE/role_app_ids.json"
E2E_TOKEN="$(APP_SET=$APP_SET "$EMULATOR_PY" -I "$EMULATOR_DIR/scripts/ci-seed-mint-apps.py" token "$EMU" "$ADMIN" "$ORG" e2e)"
echo "::add-mask::$E2E_TOKEN"
phase seed

# 2b. The test inference gateway, trusting the emulator's OIDC issuer and
# authorising the pool repositories under this emulator's halfsend org id.
if [ "$GATEWAY" = 1 ]; then
  discovery=$(curl -fsS "$EMU/.well-known/openid-configuration")
  GW_HOST=$GW_HOST GW_IP=$GW_IP STACK_TLS="$STATE/stack/tls" \
  ISSUER="$(jq -r .issuer <<<"$discovery")" JWKS_URL="$(jq -r .jwks_uri <<<"$discovery")" \
  POOL_ORG_ID="$(curl -sS -H "Authorization: token $ADMIN" "$EMU/api/v3/orgs/$ORG" | jq -r .id)" \
    "$ROOT/e2e/behaviour/emulator/gateway-stub.sh" start "$STATE/gateway"
  set -a
  # shellcheck disable=SC1091 # written by gateway-stub.sh start
  . "$STATE/gateway/gateway.env"
  set +a
  echo "::add-mask::$E2E_INFERENCE_GATEWAY_TEST_KEY"
  phase gateway
fi

# 3. fullsend's standalone mint, trusting the emulator's OIDC issuer and API.
(cd "$ROOT/cmd/mint" && go build -o "$STATE/mint" .)
hosts=""
for i in $(seq -f %02g 1 "${BEHAVIOUR_POOL_SIZE:-1}"); do hosts="$hosts,$ORG/test-repo-$i,$ORG/test-repo-$i-mint-host"; done
# The issuer exactly as the emulator writes it into tokens (it keeps an
# explicit :443), from its discovery document.
ISSUER=$(curl -sS "$EMU/.well-known/openid-configuration" | jq -r .issuer)
PORT=$MINT_PORT MINT_OIDC_ISSUER_URL="$ISSUER" MINT_GITHUB_API_URL="$EMU/api/v3" \
PEM_DIR="$STATE/pem" ROLE_APP_IDS="$(cat "$STATE/role_app_ids.json")" \
PER_REPO_WIF_REPOS='*' ALLOWED_WORKFLOW_FILES='*' WORKFLOW_HOST_REPOS="fullsend-ai/fullsend$hosts" \
  nohup "$STATE/mint" >"$STATE/mint.log" 2>&1 &
echo $! >"$STATE/mint.pid"
for _ in $(seq 1 50); do curl -s -o /dev/null "$MINT/" && break; sleep 0.2; done
phase mint

# 4. This checkout as fullsend-ai/fullsend, so the shim pinned to HEAD resolves.
# api ARGS: one emulator API call, printing the status code. A 503 (SQLite
# busy while the emulator still indexes a large push) is retried for ~60 s.
api() {
  local code i
  for i in $(seq 1 30); do
    code=$(curl -sS -o "$STATE/api-body" -w '%{http_code}' -H "Authorization: token $ADMIN" -H 'Content-Type: application/json' "$@")
    [ "$code" = 503 ] || break
    sleep $(( i < 5 ? 1 : 2 ))
  done
  echo "$code"
}
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
  # Pre-pull the sandbox images the agents pin (by digest) into the
  # runner user's rootless store once, in the background, so no agent job
  # pays the pull. The suite waits for it below.
  mapfile -t sandbox_images < <(git -C "$agents" grep -hoE 'image: *[^ ]+@sha256:[0-9a-f]+' HEAD -- harness | sed 's/image: *//' | sort -u)
  if [ "${#sandbox_images[@]}" -gt 0 ]; then
    ( for img in "${sandbox_images[@]}"; do podman pull -q "$img" >/dev/null; done ) >"$STATE/prepull.log" 2>&1 &
    prepull_pid=$!
  fi
fi

# 4c. The human-like test actors, as hack/setup-new-e2e-org.sh grants them
# on a live pool org: write and triage actors are org members holding the
# all-repository write / triage role (so access survives slot recreation);
# the outsider has no access. Their PATs are made here and die with the run.
seed_actor() { # seed_actor LOGIN [ROLE]: prints the actor's PAT
  local login=$1 role=${2:-} code role_id
  code=$(api -X POST "$EMU/api/v3/admin/users" -d "{\"login\":\"$login\",\"name\":\"$login\",\"email\":\"$login@localhost\",\"password\":\"$(openssl rand -hex 12)\"}")
  case "$code" in 201|422) ;; *) echo "::error::creating user $login: HTTP $code $(head -c 300 "$STATE/api-body")" >&2; return 1 ;; esac
  code=$(api -X POST "$EMU/api/v3/admin/tokens" -d "{\"login\":\"$login\",\"name\":\"behaviour\",\"scopes\":[\"repo\",\"read:org\",\"workflow\"]}")
  [ "$code" = 201 ] || { echo "::error::token for $login: HTTP $code $(head -c 300 "$STATE/api-body")" >&2; return 1; }
  local pat; pat=$(jq -r '.token // .raw // empty' "$STATE/api-body")
  [ -n "$pat" ] || { echo "::error::token for $login: no token in response" >&2; return 1; }
  if [ -n "$role" ]; then
    code=$(api -X PUT "$EMU/api/v3/orgs/$ORG/memberships/$login" -d '{"role":"member"}')
    case "$code" in 200|201) ;; *) echo "::error::membership for $login: HTTP $code $(head -c 300 "$STATE/api-body")" >&2; return 1 ;; esac
    code=$(api "$EMU/api/v3/orgs/$ORG/organization-roles")
    role_id=$(jq -r --arg n "all_repo_$role" '.roles[] | select(.name == $n) | .id' "$STATE/api-body")
    [ -n "$role_id" ] || { echo "::error::no all_repo_$role role in $ORG" >&2; return 1; }
    code=$(api -X PUT "$EMU/api/v3/orgs/$ORG/organization-roles/users/$login/$role_id")
    [ "$code" = 204 ] || { echo "::error::all_repo_$role for $login: HTTP $code $(head -c 300 "$STATE/api-body")" >&2; return 1; }
  fi
  echo "$pat"
}
TEST_ACTOR_WRITE_PAT=$(seed_actor fstest-write write)
TEST_ACTOR_TRIAGE_PAT=$(seed_actor fstest-triage triage)
TEST_ACTOR_OUTSIDER_PAT=$(seed_actor fstest-outsider)
for pat in "$TEST_ACTOR_WRITE_PAT" "$TEST_ACTOR_TRIAGE_PAT" "$TEST_ACTOR_OUTSIDER_PAT"; do echo "::add-mask::$pat"; done
export TEST_ACTOR_WRITE_PAT TEST_ACTOR_TRIAGE_PAT TEST_ACTOR_OUTSIDER_PAT
phase actors

if [ -n "${prepull_pid:-}" ]; then
  wait "$prepull_pid" || echo "::warning::sandbox image pre-pull failed; agent jobs pull on demand ($(tail -c 300 "$STATE/prepull.log"))"
  phase prepull-wait
fi

# 5. The suite, emulator mode.
status=0
(
  cd "$ROOT"
  unset GITHUB_TOKEN GITHUB_ACTIONS
  export ENVIRONMENT=emulator BEHAVIOUR_GITHUB_URL="$EMU" GH_TOKEN="$E2E_TOKEN" \
    FULLSEND_MINT_URL="$MINT" BEHAVIOUR_APP_SET="$APP_SET" BEHAVIOUR_CAPABILITIES="$CAPABILITIES" \
    BEHAVIOUR_POOL_SIZE="${BEHAVIOUR_POOL_SIZE:-1}" \
    GODOG_CONCURRENCY="${GODOG_CONCURRENCY:-1}" BEHAVIOUR_ARTIFACT_DIR="$STATE/bt-artifacts" \
    BEHAVIOUR_RUN_POLL_INTERVAL="${BEHAVIOUR_RUN_POLL_INTERVAL:-3s}" \
    BEHAVIOUR_NEGATIVE_SETTLE="${BEHAVIOUR_NEGATIVE_SETTLE:-20s}"
  if [ "$SUITE" = playback ]; then
    go test -tags playback -count=1 -timeout "${SUITE_TIMEOUT:-40m}" -v ./e2e/behaviour/ -run TestPlaybackSuite
  else
    BEHAVIOUR_FEATURES="${FEATURES[*]}" \
      go test -tags behaviour -count=1 -timeout "${SUITE_TIMEOUT:-40m}" -v ./e2e/behaviour/ -run TestBehaviourSuite
  fi
) 2>&1 | tee "$STATE/bt-run.log" || status=$?
phase suite
exit "$status"
