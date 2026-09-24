#!/bin/bash
# The every-scenario run (docs/plan/M5.md › Tests): one persistent stream, every Start button in turn, and per
# scenario the assertions a dead stream, a silent checker or a red light fails. Usage:
#   scripts/m5-panel-check.sh [host:port] [scenario ...]
# Scenarios default to all nine starts, short ones first (slow_kms_naive expects L1 red in its naive half). The two no-cache runs expect red lights (L1 in the band
# run; L4 in the global run, where L1 is not judged) while they run and green once their tail has ended. Exit status is the number
# of failing scenarios. Not a Go test: it needs a running binary and about 13 minutes for the full set.
set -u
H=${1:-localhost:8080}; shift || true
SCEN=("$@"); [ ${#SCEN[@]} -eq 0 ] && SCEN=(provider_blip key_revocation slow_kms slow_kms_naive tenant_surge no_cache provider_outage global_surge no_cache_surge)
LIGHTS=${LIGHTS:-S1 S2 S3 S4 L1 L4}
LOG=$(mktemp)
curl -sN "http://$H/v1/stream" > "$LOG" &
STREAM=$!
trap 'kill $STREAM 2>/dev/null; rm -f "$LOG"' EXIT
frames() { grep -c '^data:' "$LOG"; }
last() { grep '^data:' "$LOG" | tail -1 | cut -c7-; }
j() { last | jq -r "$1"; }
sleep 10 # a baseline under traffic
[ "$(frames)" -ge 10 ] || { echo "FAIL stream: $(frames) frames in 10 s"; exit 1; }
# duration each scenario is left to run before the assertions (its phases plus enough tail to be judged)
dur() { case $1 in provider_blip) echo 25;; key_revocation) echo 45;; slow_kms) echo 75;; slow_kms_naive) echo 80;; tenant_surge) echo 75;; provider_outage) echo 90;; global_surge) echo 150;; no_cache) echo 80;; no_cache_surge) echo 190;; *) echo 60;; esac; }
allowed() { case $1 in no_cache|slow_kms_naive) echo "L1";; no_cache_surge) echo "L4";; *) echo "";; esac; }
fails=0
for sc in "${SCEN[@]}"; do
  for i in $(seq 1 120); do [ "$(j '.scenario.name')" = "" ] && break; sleep 1; done
  if [ "$(j '.scenario.name')" != "" ]; then echo "FAIL $sc: a scenario is still running before start"; fails=$((fails+1)); continue; fi
  start=$(frames)
  code=$(curl -s -o /dev/null -w '%{http_code}' -XPOST "http://$H/v1/scenarios/$sc/start")
  if [ "$code" != "200" ]; then echo "FAIL $sc: start answered $code"; fails=$((fails+1)); continue; fi
  d=$(dur "$sc")
  sleep "$d"
  if [ "$sc" = key_revocation ]; then curl -s -o /dev/null -XPOST "http://$H/v1/scenarios/$sc/stop"; sleep 15; d=$((d+15)); fi
  if [ "$(j '.scenario.name')" != "" ]; then curl -s -o /dev/null -XPOST "http://$H/v1/scenarios/$sc/stop"; sleep 2; fi
  end=$(frames)
  n=$((end-start))
  need=$(( d * 2 * 8 / 10 ))
  bad=""
  [ "$n" -ge "$need" ] || bad="$bad frames=$n<$need"
  t=$(j '.t')
  for id in $LIGHTS; do
    at=$(j ".invariants[\"$id\"].at // empty")
    fresh=2; [ "$id" = S1 ] && fresh=7 # S1's full scan runs every CanaryFullScan (5 s); the others every pass
    if [ -z "$at" ]; then bad="$bad $id=absent"; elif [ $(( (t-at) / 1000 )) -gt $fresh ]; then bad="$bad $id=stale($(( (t-at)/1000 ))s)"; fi
  done
  allow=$(allowed "$sc")
  reds=$(sed -n "$((start+1)),$((end))p" <(grep '^data:' "$LOG") | cut -c7- | jq -r '.invariants // {} | to_entries[] | select(.value.ok==false) | .key' | sort | uniq -c | awk '{print $2"="$1}' | tr '\n' ' ')
  for r in $reds; do
    id=${r%%=*}
    case " $allow " in *" $id "*) ;; *) bad="$bad red:$r";; esac
  done
  if [ -n "$allow" ]; then # the expected reds must be green again once the run has ended
    lastReds=$(j '.invariants // {} | to_entries[] | select(.value.ok==false) | .key' | tr '\n' ' ')
    [ -z "$lastReds" ] || bad="$bad still-red-after:$lastReds"
  fi
  if [ -z "$bad" ]; then echo "PASS $sc: $n frames, lights fresh, reds: ${reds:-none}${allow:+ (allowed: $allow)}"; else echo "FAIL $sc:$bad"; fails=$((fails+1)); fi
done
exit $fails
