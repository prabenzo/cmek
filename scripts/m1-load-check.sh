#!/usr/bin/env bash
# usage: scripts/m1-load-check.sh /path/to/stream.sse [N=40]  → means over the last N frames, then /health
# The stream file is raw SSE frames captured with `curl -sN localhost:8080/v1/stream`.
F=${1:?stream file}; N=${2:-40}
grep '^data:' "$F" | tail -n "$N" | perl -ne '
  $n++; $a+=$1 if /"accepted":([\d.]+)/; $d+=$1 if /"delivered_ps":([\d.]+)/; $i+=$1 if /"internal":([\d.]+)/;
  $k+=$1 if /"kms_ps":\{"ok":([\d.]+)/; $t=$1 if /"total":(\d+)/; $s=$1 if /"by_state":\[([^\]]+)\]/;
  END { printf "frames=%d accepted_ps=%.0f delivered_ps=%.0f internal_ps=%.2f kms_ok_ps=%.1f backlog_total=%d by_state=[%s]\n",
        $n, $a/$n, $d/$n, $i/$n, $k/$n, $t, $s }'
curl -s "localhost:${PORT:-8080}/health"; echo
