#!/usr/bin/env bash
# collect.sh: run the conformance probe workflows on one GitHub (real or
# emulator) and save what its REST API reports.
#
#   API=https://api.github.com TOKEN=... REPO=owner/name OUT=dir collect.sh
#
# The probe workflows (repo/.github/workflows/p-*.yml) must already be on the
# default branch "main". Request budget: about 150 calls.
set -uo pipefail
: "${API:?}" "${TOKEN:?}" "${REPO:?}" "${OUT:?}"
mkdir -p "$OUT"
LOG="$OUT/collect.log"
: >"$LOG"
note() { echo "$(date -u +%H:%M:%S) $*" | tee -a "$LOG"; }

# api METHOD PATH [JSON] -> body in $OUT/.body, prints the HTTP code.
api() {
  local m="$1" p="$2" d="${3:-}"
  local args=(-sS -o "$OUT/.body" -w '%{http_code}' -X "$m" -H "Authorization: Bearer $TOKEN"
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28')
  [ -n "$d" ] && args+=(-H 'Content-Type: application/json' -d "$d")
  curl "${args[@]}" "$API$p" 2>>"$LOG" || echo 000
}
save() { cp "$OUT/.body" "$OUT/$1"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# dispatch WF: dispatch and print the new run's id (waits up to ~60 s for it).
dispatch() {
  local wf="$1" since code id=""
  since="$(now)"
  sleep 1
  code="$(api POST "/repos/$REPO/actions/workflows/$wf/dispatches" '{"ref":"main"}')"
  note "dispatch $wf -> HTTP $code"
  cp "$OUT/.body" "$OUT/dispatch-$wf.$code.json"
  [ "$code" = 204 ] || { echo ""; return; }
  for _ in 1 2 3 4 5 6; do
    sleep 10
    api GET "/repos/$REPO/actions/workflows/$wf/runs?per_page=5&event=workflow_dispatch" >/dev/null
    id="$(jq -r --arg s "$since" '[.workflow_runs[] | select(.created_at >= $s)] | sort_by(.id) | last | .id // empty' "$OUT/.body")"
    [ -n "$id" ] && break
  done
  note "run of $wf: ${id:-none}"
  echo "$id"
}

# job_status RUN NAME -> status of that job ("" if not listed).
job_status() {
  api GET "/repos/$REPO/actions/runs/$1/jobs?filter=latest" >/dev/null
  jq -r --arg n "$2" '[.jobs[] | select(.name == $n)][0].status // ""' "$OUT/.body"
}

declare -A RUN

# 1. needs: list jobs while a runs and b, c wait on it.
RUN[p-needs]="$(dispatch p-needs.yml)"
if [ -n "${RUN[p-needs]}" ]; then
  for i in 1 2 3 4 5 6 7 8 9; do
    st="$(job_status "${RUN[p-needs]}" a)"
    if [ "$st" = in_progress ]; then
      sleep 10
      api GET "/repos/$REPO/actions/runs/${RUN[p-needs]}/jobs?filter=latest" >/dev/null; save needs-midrun-jobs.json
      api GET "/repos/$REPO/actions/runs/${RUN[p-needs]}" >/dev/null; save needs-midrun-run.json
      note "needs snapshot taken (round $i)"
      break
    fi
    sleep 10
  done
fi

# 2. workflow-level concurrency: second dispatch once the first is running.
RUN[p-runconc-1]="$(dispatch p-runconc.yml)"
for _ in 1 2 3 4 5 6 7 8 9; do
  [ "$(job_status "${RUN[p-runconc-1]}" slow)" = in_progress ] && break
  sleep 10
done
RUN[p-runconc-2]="$(dispatch p-runconc.yml)"

# 3. job-level concurrency: second dispatch once the first's slow job runs.
RUN[p-jobconc-1]="$(dispatch p-jobconc.yml)"
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  [ "$(job_status "${RUN[p-jobconc-1]}" slow)" = in_progress ] && break
  sleep 10
done
RUN[p-jobconc-2]="$(dispatch p-jobconc.yml)"
# Mid-overlap snapshot of both runs' jobs.
sleep 15
for k in p-jobconc-1 p-jobconc-2; do
  api GET "/repos/$REPO/actions/runs/${RUN[$k]}/jobs?filter=latest" >/dev/null; save "$k-midrun-jobs.json"
done

# 4. the rest.
for wf in p-skipped p-reusable p-unresolved p-notice p-context p-artifact p-oidc; do
  RUN[$wf]="$(dispatch "$wf.yml")"
done

# 5. wait for every run to complete: one list call per round, 20 s apart, 20 min cap.
ids=()
for k in "${!RUN[@]}"; do [ -n "${RUN[$k]}" ] && ids+=("${RUN[$k]}"); done
for _ in $(seq 1 60); do
  api GET "/repos/$REPO/actions/runs?per_page=50" >/dev/null
  left="$(jq -r --argjson ids "$(printf '%s\n' "${ids[@]}" | jq -s 'map(tonumber)')" \
    '[.workflow_runs[] | select(.id as $i | $ids | index($i)) | select(.status != "completed")] | length' "$OUT/.body")"
  note "runs not completed: $left"
  [ "$left" = 0 ] && break
  sleep 20
done

# 6. save each run, its jobs, logs and artifacts.
for k in "${!RUN[@]}"; do
  id="${RUN[$k]}"
  [ -n "$id" ] || { note "no run for $k"; continue; }
  d="$OUT/$k"; mkdir -p "$d"
  api GET "/repos/$REPO/actions/runs/$id" >/dev/null; cp "$OUT/.body" "$d/run.json"
  api GET "/repos/$REPO/actions/runs/$id/jobs?filter=all" >/dev/null; cp "$OUT/.body" "$d/jobs.json"
  api GET "/repos/$REPO/actions/runs/$id/artifacts" >/dev/null; cp "$OUT/.body" "$d/artifacts.json"
  for jid in $(jq -r '.jobs[]?.id' "$d/jobs.json"); do
    curl -sSL -o "$d/job-$jid.log" -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
      "$API/repos/$REPO/actions/jobs/$jid/logs" >>"$d/log-codes.txt" 2>>"$LOG"
  done
  if [ "$k" = p-notice ]; then
    for jid in $(jq -r '.jobs[]?.id' "$d/jobs.json"); do
      code="$(api GET "/repos/$REPO/check-runs/$jid/annotations")"
      cp "$OUT/.body" "$d/annotations-$jid.$code.json"
    done
  fi
done
printf '%s\n' "$(for k in "${!RUN[@]}"; do echo "$k ${RUN[$k]}"; done | sort)" >"$OUT/runs.txt"
rm -f "$OUT/.body"
note "done"
