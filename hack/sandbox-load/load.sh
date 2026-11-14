#!/usr/bin/env bash
# Measure N concurrent OpenShell sandboxes on one VM, image pre-pulled.
# Usage: load.sh N OUTDIR
# Each sandbox is created the way fullsend creates one (internal/sandbox
# createOnce), gets the repo uploaded, runs a short shell workload, then is
# deleted. vmstat samples CPU and memory every second throughout.
set -euo pipefail

n="$1"
out="$2"
image="${SANDBOX_IMAGE:-ghcr.io/fullsend-ai/fullsend-sandbox:latest}"
mkdir -p "$out"

ts() { date +%s.%N; }

one() {
  local i="$1" name="bt-load-${n}-$1" t0 t1 t2 t3 t4
  t0=$(ts)
  openshell sandbox create --name "$name" --keep --no-auto-providers --no-tty \
    --from "$image" --detach -- sleep infinity >"$out/create-$i.log" 2>&1 || true
  until openshell sandbox get "$name" 2>/dev/null | grep -Eq 'Phase:.*Ready'; do
    if [[ $(echo "$(ts) - $t0 > 180" | bc) == 1 ]]; then echo "$i timeout" >>"$out/errors.txt"; echo fail >"$out/ready-$i"; return 1; fi
    sleep 0.5
  done
  t1=$(ts)
  tar -czf "$out/up-$i.tgz" -C "$GITHUB_WORKSPACE" --exclude=.git --exclude=web --exclude=node_modules .
  openshell sandbox upload --no-git-ignore "$name" "$out/up-$i.tgz" /tmp/ >"$out/upload-$i.log" 2>&1 || echo "$i upload failed" >>"$out/errors.txt"
  t2=$(ts)
  openshell sandbox exec --name "$name" --no-tty --timeout 120 -- sh -c \
    'cd /tmp && tar -xzf up-*.tgz 2>/dev/null; find . -type f | wc -l; for k in 1 2 3; do grep -rc func . >/dev/null 2>&1; done; echo ok' \
    >"$out/exec-$i.log" 2>&1 || echo "$i exec failed" >>"$out/errors.txt"
  t3=$(ts)
  echo "$t3" >"$out/ready-$i"
  # Hold until every sandbox has finished its workload, so the steady-state
  # sample sees all N alive at once.
  while [[ $(find "$out" -name "ready-*" | wc -l) -lt $n ]]; do sleep 0.5; done
  sleep 15
  openshell sandbox delete "$name" >"$out/delete-$i.log" 2>&1 || true
  t4=$(ts)
  printf '%s %.1f %.1f %.1f %.1f\n' "$i" "$(echo "$t1-$t0" | bc)" "$(echo "$t2-$t1" | bc)" "$(echo "$t3-$t2" | bc)" "$(echo "$t4-$t3" | bc)" >>"$out/phases.txt"
}

vmstat -n -t 1 >"$out/vmstat.txt" &
vm=$!
free -m >"$out/free-before.txt"
start=$(ts)
for i in $(seq 1 "$n"); do one "$i" & done
# Snapshot memory and per-container stats once all N are alive.
until [[ $(find "$out" -name "ready-*" | wc -l) -ge $n ]] || [[ $(echo "$(ts) - $start > 300" | bc) == 1 ]]; do sleep 1; done
sleep 5
free -m >"$out/free-peak.txt"
podman stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' >"$out/podman-stats.txt" 2>&1 || true
ps -eo rss,comm --sort=-rss | head -15 >"$out/ps-top.txt"
for p in $(jobs -p); do [[ "$p" == "$vm" ]] || wait "$p" || true; done
end=$(ts)
kill "$vm" || true
free -m >"$out/free-after.txt"
printf 'n=%s wall=%.1f\n' "$n" "$(echo "$end-$start" | bc)" >"$out/summary.txt"
echo "== n=$n: id create-to-ready upload exec hold+delete (s)"
sort -n "$out/phases.txt"
cat "$out/summary.txt"
echo "== free at peak"; cat "$out/free-peak.txt"
echo "== podman stats at peak"; cat "$out/podman-stats.txt"
echo "== top RSS"; cat "$out/ps-top.txt"
echo "== vmstat peak: min idle %, max run queue, min free MB"
awk 'NR>2 {if (min_id=="" || $15<min_id) min_id=$15; if ($1>max_r) max_r=$1; if (min_free=="" || $4<min_free) min_free=$4} END {print min_id, max_r, int(min_free/1024)}' "$out/vmstat.txt"
if [[ -f "$out/errors.txt" ]]; then echo "== errors"; cat "$out/errors.txt"; fi
