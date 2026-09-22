#!/usr/bin/env bash
# M0 acceptance check on the live Cloud Run URL (docs/plan/M0.md › Acceptance check).
# usage: scripts/m0-ticker-check.sh URL [A|B|AB]   (default AB)
#   A: stream 60 s; ticks must advance ≈ 2/s (PASS ≥ 100; 60–99 rerun once; < 60 → D9 fallback)
#   B: idle 30 s with NO viewer (close every tab and stream first; B refuses to run otherwise); ticks must advance ≤ 20
set -u; URL=${1:?usage: $0 URL [A|B|AB]}; PART=${2:-AB}
hz() { curl -sf "$URL/health" || { echo "FAIL: /health not 200 (check --allow-unauthenticated)"; return 1; }; }
field() { grep -oE "\"$1\":[^,}]+" | cut -d: -f2 | tr -d '"'; }
hz  || exit 1   # absorbs the cold start
if [[ $PART == *A* ]]; then echo "A: streaming 60 s"
  curl -sN --max-time 62 "$URL/v1/stream" | perl -MTime::HiRes=time -ne '
    next unless /"tick":(\d+)/; $t=$1; $now=time;
    if (defined $prev) { $g=$now-$prev; $max=$g if $g>$max;
      if ($g>1.5) { $over++; printf "A: gap %.2fs tick_delta=%d (%s)\n", $g, $t-$pt, ($t-$pt)>$g ? "buffering" : "starved" } }
    $prev=$now; $pt=$t; $first//=$t; $last=$t; $n++;
    END { printf "A: frames=%d ticks_advanced=%d max_gap_s=%.2f gaps_over_1.5s=%d\n", $n, ($last//0)-($first//0), $max//0, $over//0 }'
fi
if [[ $PART == *B* ]]; then
  for i in $(seq 10); do H=$(hz) || exit 1; [[ $(echo "$H" | field viewers) == 0 ]] && break; sleep 1; done
  if [[ $(echo "$H" | field viewers) != 0 ]]; then
    echo "B: NOT RUN: viewers=$(echo "$H" | field viewers) streams are still open, so a request is in flight and CPU is allocated; close every browser tab on the page and every curl on /v1/stream, confirm /health shows \"viewers\":0, then rerun: $0 $URL B"; exit 2
  fi
  T1=$(echo "$H" | field ticks); W1=$(echo "$H" | field world)
  [[ $T1 =~ ^[0-9]+$ ]] || { echo "FAIL: health unparsable: $H"; exit 1; }
  echo "B: idle 30 s (viewers=$(echo "$H" | field viewers))"; sleep 30
  H=$(hz) || exit 1; T2=$(echo "$H" | field ticks); W2=$(echo "$H" | field world)
  echo "B: idle_ticks=$((T2-T1)) world=$W1->$W2 viewers=$(echo "$H" | field viewers)"
fi
