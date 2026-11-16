#!/usr/bin/env bash
# Probe: where the VM's time goes while the suite runs, once a second.
#
#   e2e/behaviour/emulator/sample-io.sh OUT_DIR &
#
# OUT_DIR/io-samples.log, one line per second (percentages of all CPUs):
#   usr sys iow stl idl   CPU split from /proc/stat
#   run blk               procs_running and procs_blocked (D state, mostly I/O)
#   psi_cpu psi_io psi_iofull psi_mem
#                         share of the second with some task stalled
#                         (pressure stall information, /proc/pressure/*)
#   per disk: <dev>_util% <dev>_rMB <dev>_wMB <dev>_wops <dev>_aqu
#                         busy share, MB read/written, write ops and average
#                         queue length from /proc/diskstats
# OUT_DIR/dstate.log: every second, the commands in D state with their counts.
# OUT_DIR/disk-layout.log: block devices, mounts and where the state lives.
set -uo pipefail

out="$1"
mkdir -p "$out"
log="$out/io-samples.log"
dlog="$out/dstate.log"

{
  echo "== nproc $(nproc)"
  lsblk -o NAME,SIZE,ROTA,TYPE,MOUNTPOINTS 2>&1
  echo "== df"
  df -hT / /tmp "${RUNNER_TEMP:-/tmp}" "$out" /var/lib/containers "$HOME" 2>&1 | sort -u
  echo "== schedulers"
  for q in /sys/block/*/queue/scheduler; do echo "$q: $(cat "$q")"; done
  echo "== pressure available"
  ls /proc/pressure 2>&1
} >"$out/disk-layout.log" 2>&1

disks=$(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2 == "disk" {print $1}' | tr '\n' ' ')

psi_total() { # $1 file, $2 some|full -> cumulative stall microseconds
  awk -v k="$2" '$1 == k {for (i = 2; i <= NF; i++) if ($i ~ /^total=/) {sub("total=", "", $i); print $i}}' "$1" 2>/dev/null || echo 0
}

declare -A prev
snap() {
  local _ u n s i w q sq st
  read -r _ u n s i w q sq st _ </proc/stat
  cur[cpu_usr]=$((u + n)) cur[cpu_sys]=$((s + q + sq)) cur[cpu_iow]=$w cur[cpu_stl]=$st cur[cpu_idl]=$i
  cur[psi_cpu]=$(psi_total /proc/pressure/cpu some)
  cur[psi_io]=$(psi_total /proc/pressure/io some)
  cur[psi_iofull]=$(psi_total /proc/pressure/io full)
  cur[psi_mem]=$(psi_total /proc/pressure/memory some)
  cur[t]=$(date +%s%N)
  local d
  for d in $disks; do
    # fields: 3 rd_ios 6 rd_sectors 7 wr_ios 10 wr_sectors 13 io_ticks 14 weighted_ms
    read -r r rs w ws tk wq < <(awk -v d="$d" '$3 == d {print $4, $6, $8, $10, $13, $14}' /proc/diskstats)
    cur[${d}_r]=${r:-0} cur[${d}_rs]=${rs:-0} cur[${d}_w]=${w:-0} cur[${d}_ws]=${ws:-0} cur[${d}_tk]=${tk:-0} cur[${d}_wq]=${wq:-0}
  done
}

header="time usr sys iow stl idl run blk psi_cpu psi_io psi_iofull psi_mem"
for d in $disks; do header="$header ${d}_util ${d}_rMB ${d}_wMB ${d}_wops ${d}_aqu"; done
echo "$header" >"$log"

declare -A cur
snap
for k in "${!cur[@]}"; do prev[$k]=${cur[$k]}; done
while :; do
  sleep 1
  snap
  dt_us=$(((cur[t] - prev[t]) / 1000))
  [ "$dt_us" -gt 0 ] || dt_us=1
  cpu_all=$((cur[cpu_usr] + cur[cpu_sys] + cur[cpu_iow] + cur[cpu_stl] + cur[cpu_idl] - prev[cpu_usr] - prev[cpu_sys] - prev[cpu_iow] - prev[cpu_stl] - prev[cpu_idl]))
  [ "$cpu_all" -gt 0 ] || cpu_all=1
  line="$(date -u +%H:%M:%S)"
  for k in cpu_usr cpu_sys cpu_iow cpu_stl cpu_idl; do
    line="$line $((100 * (cur[$k] - prev[$k]) / cpu_all))"
  done
  run=$(awk '/^procs_running/ {print $2}' /proc/stat)
  blk=$(awk '/^procs_blocked/ {print $2}' /proc/stat)
  line="$line $run $blk"
  for k in psi_cpu psi_io psi_iofull psi_mem; do
    line="$line $((100 * (cur[$k] - prev[$k]) / dt_us))"
  done
  dt_ms=$((dt_us / 1000))
  [ "$dt_ms" -gt 0 ] || dt_ms=1
  for d in $disks; do
    util=$((100 * (cur[${d}_tk] - prev[${d}_tk]) / dt_ms))
    rmb=$(((cur[${d}_rs] - prev[${d}_rs]) / 2048))
    wmb=$(((cur[${d}_ws] - prev[${d}_ws]) / 2048))
    wops=$((cur[${d}_w] - prev[${d}_w]))
    aqu=$(awk -v a="$((cur[${d}_wq] - prev[${d}_wq]))" -v b="$dt_ms" 'BEGIN {printf "%.1f", a / b}')
    line="$line $util $rmb $wmb $wops $aqu"
  done
  echo "$line" >>"$log"
  dl=$(ps -eo state=,comm= 2>/dev/null | awk '$1 == "D" {c[$2]++} END {for (k in c) printf "%s=%d ", k, c[k]}')
  [ -n "$dl" ] && echo "$(date -u +%H:%M:%S) $dl" >>"$dlog"
  for k in "${!cur[@]}"; do prev[$k]=${cur[$k]}; done
done
