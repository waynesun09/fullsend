#!/usr/bin/env bash
# Sample the VM while the behaviour suite runs, so a run shows whether its
# concurrency fits the machine: CPU, memory, run queue, and how many
# sandboxes, runner jobs and emulator requests were live at the time.
#
#   e2e/behaviour/emulator/sample-vm.sh OUT_DIR EMULATOR_PID_FILE &
#
# Writes OUT_DIR/vm-samples.log every SAMPLE_INTERVAL seconds (default 5)
# and a one-line peak summary to OUT_DIR/vm-peak.log when it is stopped.
# EMULATOR_PROFILE=1 also records a py-spy profile of the emulator to
# OUT_DIR/emulator-profile.json (speedscope format; needs sudo and
# EMULATOR_PY, the emulator's virtualenv python).
set -uo pipefail

out="$1"
pid_file="$2"
interval="${SAMPLE_INTERVAL:-5}"
log="$out/vm-samples.log"
mkdir -p "$out"

emu_pid=""
[ -f "$pid_file" ] && emu_pid=$(cat "$pid_file")

if [ "${EMULATOR_PROFILE:-0}" = 1 ] && [ -n "$emu_pid" ]; then
  py="${EMULATOR_PY:-python3}"
  # The job points SSL_CERT_FILE at the emulator's CA; PyPI needs the system one.
  if env -u SSL_CERT_FILE -u REQUESTS_CA_BUNDLE "$py" -m pip install -q py-spy >"$out/py-spy.log" 2>&1; then
    # shellcheck disable=SC2024 # the log is the runner user's, not root's
    sudo "$(dirname "$py")/py-spy" record --pid "$emu_pid" --subprocesses --nonblocking \
      --format speedscope --output "$out/emulator-profile.json" >>"$out/py-spy.log" 2>&1 &
    spy=$!
  fi
fi

cpu_busy() { # prints busy % since the previous call
  local _ user nice system idle iowait irq softirq steal
  read -r _ user nice system idle iowait irq softirq steal _ </proc/stat
  local total=$((user + nice + system + idle + iowait + irq + softirq + steal))
  local idle_all=$((idle + iowait))
  if [ -n "${prev_total:-}" ] && [ "$total" -gt "$prev_total" ]; then
    echo $((100 * ((total - prev_total) - (idle_all - prev_idle)) / (total - prev_total)))
  else
    echo 0
  fi
  prev_total=$total prev_idle=$idle_all
}

peak_cpu=0 peak_mem=0 peak_load=0 peak_sandboxes=0 samples=0 busy_samples=0
summary() {
  echo "samples=$samples cpu_busy_peak=${peak_cpu}% cpu_busy_ge90_samples=$busy_samples mem_used_peak=${peak_mem}MB load1_peak=$peak_load sandboxes_peak=$peak_sandboxes" >"$out/vm-peak.log"
  if [ -n "${spy:-}" ]; then
    # py-spy writes the profile on SIGINT; give it 30 s, then stop waiting.
    sudo pkill -INT -f "py-spy record --pid $emu_pid" 2>/dev/null
    for _ in $(seq 30); do kill -0 "$spy" 2>/dev/null || break; sleep 1; done
  fi
  exit 0
}
trap summary TERM INT

echo "time cpu_busy% load1 mem_used_MB mem_avail_MB swap_used_MB sandboxes emu_rss_MB" >"$log"
cpu_busy >/dev/null
while :; do
  sleep "$interval"
  busy=$(cpu_busy)
  read -r load1 _ </proc/loadavg
  read -r used avail < <(free -m | awk '/^Mem:/ {print $3, $7}')
  swap=$(free -m | awk '/^Swap:/ {print $3}')
  # podman ps can block on the store lock while containers start or stop.
  boxes=$(timeout 5 podman ps -q --filter name=openshell-default 2>/dev/null | wc -l)
  rss=0
  [ -n "$emu_pid" ] && rss=$(($(ps -o rss= -p "$emu_pid" 2>/dev/null || echo 0) / 1024))
  echo "$(date -u +%H:%M:%S) $busy $load1 $used $avail $swap $boxes $rss" >>"$log"
  samples=$((samples + 1))
  [ "$busy" -gt "$peak_cpu" ] && peak_cpu=$busy
  [ "$busy" -ge 90 ] && busy_samples=$((busy_samples + 1))
  [ "$used" -gt "$peak_mem" ] && peak_mem=$used
  [ "$boxes" -gt "$peak_sandboxes" ] && peak_sandboxes=$boxes
  if awk -v a="$load1" -v b="$peak_load" 'BEGIN {exit !(a > b)}'; then peak_load=$load1; fi
done
