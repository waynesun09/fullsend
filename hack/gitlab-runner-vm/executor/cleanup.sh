#!/usr/bin/env bash
# GitLab Runner custom executor — cleanup stage.
# Stops and removes the job container. Always succeeds.
# -e intentionally omitted — cleanup must not abort on individual failures.
set -uo pipefail

# Job identity comes from the runner-written JOB_RESPONSE_FILE, never from
# CUSTOM_ENV_CI_JOB_ID: CUSTOM_ENV_* values are job-controlled, so a job could
# name a concurrent job's container/state file and have this stage act on it.
# The runner sets JOB_RESPONSE_FILE for every stage and removes it only after
# cleanup has run.
resolve_job_id() {
  [ -n "${JOB_RESPONSE_FILE:-}" ] && [ -r "${JOB_RESPONSE_FILE}" ] || return 1
  python3 -c '
import json, sys
v = json.load(open(sys.argv[1]))["id"]
if not isinstance(v, int) or v <= 0:
    sys.exit(1)
print(v)
' "${JOB_RESPONSE_FILE}"
}
JOB_ID=$(resolve_job_id) || {
  echo "WARN: could not read job id from JOB_RESPONSE_FILE — nothing to clean up"
  exit 0
}

STATE_DIR="${HOME}/.local/state/gitlab-runner"
STATE_FILE="${STATE_DIR}/container-${JOB_ID}"

if [ -f "${STATE_FILE}" ]; then
  CONTAINER_NAME=$(cat "${STATE_FILE}")
  if [[ "${CONTAINER_NAME}" =~ ^runner-[0-9]+$ ]]; then
    echo "Cleaning up container: ${CONTAINER_NAME}"
    podman stop --time 10 "${CONTAINER_NAME}" 2>/dev/null || true
    podman rm -f "${CONTAINER_NAME}" 2>/dev/null || true
  else
    # Skip only the podman calls — the staging copy of the gateway mTLS
    # material below must still be removed.
    echo "WARN: state file holds an unexpected container name (${CONTAINER_NAME}) — not touching podman"
  fi
  rm -f "${STATE_FILE}"
fi

OPENSHELL_STAGING="${STATE_DIR}/openshell-${JOB_ID}"
rm -rf "${OPENSHELL_STAGING}" 2>/dev/null || true
