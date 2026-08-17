#!/usr/bin/env bash
# GitLab Runner custom executor — run stage.
# Executes each build script inside the container, forwarding CI env vars.
set -uo pipefail

SCRIPT_PATH="${1:-}"
if [ -z "${SCRIPT_PATH}" ]; then
  echo "ERROR: no script path provided"
  exit "${SYSTEM_FAILURE_EXIT_CODE}"
fi
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
  echo "ERROR: could not read job id from JOB_RESPONSE_FILE"
  exit "${SYSTEM_FAILURE_EXIT_CODE}"
}
STATE_FILE="${HOME}/.local/state/gitlab-runner/container-${JOB_ID}"

if [ ! -f "${STATE_FILE}" ]; then
  echo "ERROR: container state file not found — prepare stage may have failed"
  exit "${SYSTEM_FAILURE_EXIT_CODE}"
fi

CONTAINER_NAME=$(cat "${STATE_FILE}") || exit "${SYSTEM_FAILURE_EXIT_CODE}"
if ! [[ "${CONTAINER_NAME}" =~ ^runner-[0-9]+$ ]]; then
  echo "ERROR: container state file holds an unexpected name (got: ${CONTAINER_NAME})"
  exit "${SYSTEM_FAILURE_EXIT_CODE}"
fi

# Forward CI environment variables into the container.
# GitLab Runner exposes job variables as CUSTOM_ENV_* — strip the prefix and
# pass each one as a separate --env argument. A line-delimited --env-file
# cannot carry these safely: file-type CI/CD variables (PEM material, keys)
# contain newlines, which an `env`-parsing loop truncates to the first line,
# and a continuation line beginning with CUSTOM_ENV_ would be re-parsed as an
# attacker-chosen assignment. argv preserves values verbatim, and keeps
# secrets out of a temp file on disk.
ENV_ARGS=()
while IFS= read -r name; do
  [ -n "${name}" ] || continue
  ENV_ARGS+=(--env "${name#CUSTOM_ENV_}=${!name}")
done < <(compgen -v | grep '^CUSTOM_ENV_')

# The script lives on the host — copy it into the container before executing.
BUILD_SCRIPT_DIR=$(dirname "${SCRIPT_PATH}")
podman exec "${CONTAINER_NAME}" mkdir -p "${BUILD_SCRIPT_DIR}" || exit "${SYSTEM_FAILURE_EXIT_CODE}"
podman cp "${SCRIPT_PATH}" "${CONTAINER_NAME}:${SCRIPT_PATH}" || exit "${SYSTEM_FAILURE_EXIT_CODE}"

# Translate exit codes per the custom-executor contract.
# podman reserves 125 for its own failures. 126/127 are NOT distinguishable
# from the job: bash returns 127 for a command it cannot find and 126 for one
# it cannot execute inside the job script, and podman propagates that verbatim,
# so those stay build failures. 125 is ambiguous too (the script may exit
# 125), so fall back to container liveness there.
podman exec \
  "${ENV_ARGS[@]}" \
  "${CONTAINER_NAME}" \
  bash -- "${SCRIPT_PATH}"; rc=$?
# Report the script's real status so `allow_failure:exit_codes` works. The
# runner requires a bare integer here and treats anything else as an unknown
# failure, so write nothing when the code did not come from the script.
write_build_exit_code() {
  [ -n "${BUILD_EXIT_CODE_FILE:-}" ] && printf '%s' "$1" > "${BUILD_EXIT_CODE_FILE}"
}
case $rc in
  0)   ;;
  125)
    if podman inspect --format '{{.State.Running}}' "${CONTAINER_NAME}" 2>/dev/null | grep -q true; then
      write_build_exit_code "$rc"
      exit "${BUILD_FAILURE_EXIT_CODE}"
    fi
    exit "${SYSTEM_FAILURE_EXIT_CODE}"
    ;;
  *)
    write_build_exit_code "$rc"
    exit "${BUILD_FAILURE_EXIT_CODE}"
    ;;
esac
