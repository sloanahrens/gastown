#!/usr/bin/env bash
# sample-capacity.sh <outdir> — every 5 s until killed, append one TSV row:
# epoch, load1, VM MemTotal/MemAvailable/SwapTotal/SwapFree (kB), sum of
# container memory (MiB), container count, disk MB/s. The alpine probe is not
# a gate container (slot.go gateContainerPatterns: dolt/testcontainers/ryuk).
set -uo pipefail
out="${1:?outdir}"; mkdir -p "$out"; f="$out/capacity.tsv"
[[ -s "$f" ]] || printf 'epoch\tload1\tvm_total_kb\tvm_avail_kb\tswap_total_kb\tswap_free_kb\tctr_mem_mib\tctr_count\tdisk_mbps\n' > "$f"
while :; do
  load1=$(sysctl -n vm.loadavg | awk '{print $2}')
  mem=$(docker run --rm alpine:3.20 awk '/^(MemTotal|MemAvailable|SwapTotal|SwapFree):/{printf "%s\t",$2}' /proc/meminfo 2>/dev/null)
  [[ -n "$mem" ]] || mem=$'\t\t\t\t'   # keep columns aligned when the probe fails
  ctr=$(docker stats --no-stream --format '{{.MemUsage}}' 2>/dev/null | awk '
    { v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v);
      if (u=="GiB") v*=1024; else if (u=="KiB") v/=1024; else if (u=="B") v/=1048576;
      s+=v; n++ } END { printf "%.0f\t%d", s, n }')
  disk=$(iostat -d -w 1 -c 2 | tail -1 | awk '{s=0; for(i=3;i<=NF;i+=3) s+=$i; printf "%.1f", s}')
  printf '%s\t%s\t%s%s\t%s\n' "$(date +%s)" "$load1" "$mem" "${ctr:-0	0}" "$disk" >> "$f"
  sleep 4
done
