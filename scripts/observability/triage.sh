#!/usr/bin/env bash
# Run triage: evaluate the identities in docs/METRICS_TRIAGE.md against Prometheus
# and print OK or GAP per line. Run it at the END of a load run, and during an
# incident.
#
# It answers one question the dashboards do not: not "did anything fail?" but
# "how many should there have been?". A run that settles every claim it submitted
# still lost work if it built fewer claims than it should have, and that gap looks
# exactly like success from the outside.
#
#   ./triage.sh                                  # now, last 75 minutes
#   ./triage.sh 2026-09-17T22:38:00Z 75          # a specific run
#   SETTLEMENTS="53 73" ./triage.sh ...          # also check against the chain
#
# Configuration, in order of precedence: environment, then the operator file
# (see triage.conf.example), then the defaults below, which target a local
# Tilt/localnet.
#
# Why last_over_time and not increase(): counters are per process and reset on
# restart, and a restart is normal (HA failover, rollout, OOM). increase() cannot
# see a counter born inside the window. The window must also stay INSIDE one run —
# one that reaches into the previous run adds its numbers silently.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPTS=$(cd "$HERE/.." && pwd)
CONF=${TRIAGE_CONF:-$SCRIPTS/localonly/observability/triage.conf}
# shellcheck source=/dev/null
[ -f "$CONF" ] && . "$CONF"

PROM_URL=${PROM_URL:-http://localhost:9091}
CHAIN_URL=${CHAIN_URL:-http://localhost:26657}
END=${1:-$(date -u +%FT%TZ)}

# The window is the SECOND POSITIONAL argument, or TRIAGE_MINUTES. A caller who sets
# W= gets told, not ignored: W is what this script calls the window internally, so
# passing it that way is the natural mistake, and it used to fall through to the
# 75-minute default IN SILENCE. Measured 2026-09-18: a 122-minute run was read with the
# 75-minute default, which cut the run in half and made the claims identity report
# GAP 804 != 1054; with the real window it closes at 1054. A window that is silently
# wrong turns every identity below into decoration.
W_INHERITED=${W:-}
MINUTES=${2:-${TRIAGE_MINUTES:-75}}
if [ -n "$W_INHERITED" ] && [ -z "${2:-}" ] && [ -z "${TRIAGE_MINUTES:-}" ]; then
  echo "ABORT: W=$W_INHERITED is not how the window is passed, and the default would" >&2
  echo "       have been used instead -- silently, which is the bug this guard exists" >&2
  echo "       for. Use:  triage.sh <END-ISO8601> <MINUTES>   or  TRIAGE_MINUTES=<n>" >&2
  exit 2
fi
case $MINUTES in ''|*[!0-9]*) echo "ABORT: MINUTES must be a whole number, got '$MINUTES'" >&2; exit 2;; esac
T=$(date -u -d "$END" +%s) || { echo "invalid timestamp: $END" >&2; exit 2; }
W="${MINUTES}m"
# The window's START, printed with the header: "window 122m" alone does not say whether
# it covers the run, and that is exactly what the reader has to check.
START_ISO=$(date -u -d "@$((T - MINUTES * 60))" +%FT%TZ)

failures=0

# THE CONTROL, and it is the part that matters. An empty query is not a reading
# until something says the tool looked. Three distinct causes produce the same
# empty answer:
#   1. the metric never fired    -> the value is ZERO and the run is fine
#   2. the name is misspelled    -> the SCRIPT is broken, not the run
#   3. Prometheus is unreachable -> nothing is known
# Prometheus alone cannot separate 1 from 2: a CounterVec does not exist there
# until its first use. The truth about a name lives in the source, and this script
# ships with it, so that is what gets asked.
NAMES=$(curl -s "$PROM_URL/api/v1/label/__name__/values" | jq -r '.data[]' 2>/dev/null)
[ -z "$NAMES" ] && { echo "cannot read metric names from $PROM_URL" >&2; exit 2; }

declared() { # <full metric name> -> 0 when the source declares it
  local short=${1#ha_}
  short=${short#*_}
  grep -rqs --include='*.go' "Name: *\"$short\"\|Name: *\"${short%_total}\"" "$SCRIPTS/.." 2>/dev/null
}

# v <metric> [selector] -> the summed last value of every series in the window
v() {
  local m=$1 sel=${2:-} r
  if ! grep -qx -- "$m" <<<"$NAMES"; then
    if declared "$m"; then echo 0; else echo BAD-NAME; fi
    return
  fi
  r=$(curl -s -G "$PROM_URL/api/v1/query" \
        --data-urlencode "query=sum(last_over_time(${m}${sel}[$W]))" \
        --data-urlencode "time=$T" 2>/dev/null | jq -r '.data.result[0].value[1] // "EMPTY"')
  [ "$r" = "EMPTY" ] && { echo 0; return; }
  printf '%.0f' "$r" 2>/dev/null || echo BAD-NAME
}

# vu <metric> [selector] -> the same reading, in uPOKT instead of POKT.
# The money series are recorded as POKT: miner/metrics.go does Add(cu / 1e6). So on
# a small run every term lands below 1, printf '%.0f' turns it into 0, and the
# ledger identity prints "OK 0" over 0 / 0 / 0 / 0 -- an identity closing because
# every term rounded away, which is not a reading at all. Measured 2026-09-18 on a
# 304-claim run: claimed was 0.384 POKT and the line said OK over zeros. Scaling to
# uPOKT keeps the smallest run legible and costs the big run nothing.
vu() {
  local m=$1 sel=${2:-} r
  if ! grep -qx -- "$m" <<<"$NAMES"; then
    if declared "$m"; then echo 0; else echo BAD-NAME; fi
    return
  fi
  r=$(curl -s -G "$PROM_URL/api/v1/query" \
        --data-urlencode "query=sum(last_over_time(${m}${sel}[$W])) * 1000000" \
        --data-urlencode "time=$T" 2>/dev/null | jq -r '.data.result[0].value[1] // "EMPTY"')
  [ "$r" = "EMPTY" ] && { echo 0; return; }
  printf '%.0f' "$r" 2>/dev/null || echo BAD-NAME
}

# chain_proofs_required <height> -> settled claims that required a proof, per the
# CHAIN. Our proof_requirement_required_total counts DECISIONS, not sessions: the
# requirement is decided more than once per session (measured: 976 for 500 proofs),
# so it is not a denominator. The chain is.
chain_proofs_required() {
  curl -fsS -m 20 "$CHAIN_URL/block_results?height=$1" 2>/dev/null | jq -r '
    [(.result.finalize_block_events//[])[]
     | select(.type=="pocket.tokenomics.EventClaimSettled")
     | ([.attributes[]|select(.key=="proof_requirement_int")|.value]|add)
     | select(. != "0")] | length' 2>/dev/null || echo NA
}

# identity <label> <left> <right> [tolerance]
# The tolerance exists for a measured reason, not for comfort: counters are not
# scraped at the same instant, so an identity over millions differs by units with
# nothing missing (measured: 9 out of 12.9M). It is used ONLY where the volume
# justifies it, never on claims or proofs, which are counted one at a time.
identity() {
  local label=$1 a=$2 b=$3 tol=${4:-0} d
  if [ "$a" = BAD-NAME ] || [ "$b" = BAD-NAME ]; then
    printf '  %-52s BAD-NAME  (%s vs %s) <- fix the script\n' "$label" "$a" "$b"
    failures=$((failures+1)); return
  fi
  if [ "$a" = NA ] || [ "$b" = NA ]; then
    printf '  %-52s UNKNOWN   (%s vs %s)\n' "$label" "$a" "$b"
    failures=$((failures+1)); return
  fi
  d=$((a-b)); [ "$d" -lt 0 ] && d=$((-d))
  if [ "$d" -eq 0 ]; then
    printf '  %-52s OK    %s\n' "$label" "$a"
  elif [ "$d" -le "$tol" ]; then
    printf '  %-52s OK    %s (scrape skew %s, tolerance %s)\n' "$label" "$a" "$d" "$tol"
  else
    printf '  %-52s GAP   %s != %s   (difference %s)\n' "$label" "$a" "$b" "$((a-b))"
    failures=$((failures+1))
  fi
}

# note <label> <value> <threshold> -> not a failure, something to read
note() {
  local label=$1 val=$2 lim=$3
  if [ "$val" = BAD-NAME ]; then
    printf '  %-52s BAD-NAME <- fix the script\n' "$label"; failures=$((failures+1))
  elif [ "$val" -gt "$lim" ]; then printf '  %-52s CHECK %s\n' "$label" "$val"
  else printf '  %-52s ok    %s\n' "$label" "$val"; fi
}

echo "== run triage: window $START_ISO -> $END ($W), prometheus $PROM_URL"
echo "   Check that START is at or after the run's own start: a window that reaches into"
echo "   the previous run adds its numbers, and one that starts late cuts this one in half."

# A window that reaches into a PREVIOUS run adds its numbers silently, and the only
# way to see it from here is that more than one process reported. The miner and the
# relayer label their series with their own pod name (`exported_instance`), so a
# window spanning two runs shows two of them. Measured 2026-09-18: a 75m window
# after a 44m run still covered the level-3 gate's own load, and every claim total
# came out 304 too high -- 554 instead of 250, with nothing saying so.
inst=$(curl -s -G "$PROM_URL/api/v1/query" \
        --data-urlencode "query=count by (exported_instance) (last_over_time(ha_miner_claims_submitted_total[$W]))" \
        --data-urlencode "time=$T" 2>/dev/null | jq -r '.data.result[]?.metric.exported_instance' | sort)
ni=$(grep -c . <<<"$inst")
if [ "${ni:-0}" -gt 1 ]; then
  echo "   AVISO: $ni procesos del miner reportaron en esta ventana:"
  sed 's/^/     /' <<<"$inst"
  echo "   Si son de corridas DISTINTAS, cada total de abajo suma las dos. Una sola"
  echo "   corrida con un reinicio tambien da 2, y ahi si hay que sumarlas: la"
  echo "   diferencia la dice el nombre del pod, no este script."
fi
echo
echo "1. Did we lose claims or proofs? (the only section that answers that)"
cc=$(v ha_miner_claims_created_total); cs=$(v ha_miner_claims_submitted_total)
cf=$(v ha_miner_claim_inclusion_outcome_total '{outcome="on_chain_found"}')
pf=$(v ha_miner_proof_inclusion_outcome_total '{outcome="on_chain_found"}')
identity "claims built == claims submitted"        "$cc" "$cs"
identity "claims built == found on chain"          "$cc" "$cf"
if [ -n "${SETTLEMENTS:-}" ]; then
  req=0
  for h in $SETTLEMENTS; do
    n=$(chain_proofs_required "$h")
    [ "$n" = NA ] && { req=NA; break; }
    req=$((req+n))
  done
  identity "proofs the chain required == found on chain" "$req" "$pf"
else
  printf '  %-52s NOT CHECKED (pass SETTLEMENTS="53 73")\n' "proofs the chain required"
fi
# The resend path, and the denominator FIRST. A rebroadcast is not the defect: it
# is the path a restarted miner takes to re-send a pending proof, and what was
# fixed was the chain REJECTING it. So "zero rejections" is a reading only once
# the path actually ran -- zero rejections over zero attempts says nothing at all.
# That is not hypothetical: on 2026-09-18 a success criterion for exactly this fix
# was written as "rebroadcasts == 0", which would have declared victory for a run
# that never exercised the fix. The rejection lands in result="error", because the
# switch in miner/inclusion_reconciler.go separates only ErrTxWindowExpired and
# ErrTxAlreadyQueued and a CheckTx refusal falls through to the default.
rb=$(v ha_miner_proof_rebroadcasts_total)
if [ "$rb" = BAD-NAME ] || [ "$rb" = NA ]; then
  printf '  %-52s %s <- fix the script or the endpoint\n' "proof resends attempted" "$rb"
  failures=$((failures+1))
elif [ "$rb" -eq 0 ]; then
  # Not a failure: a run with no restart legitimately resends nothing. But it must
  # be loud, because it means this section has no denominator to divide by.
  printf '  %-52s NO-DENOM  0 attempts, so a zero rejection count here\n' "proof resends attempted"
  printf '  %-52s           proves nothing: the path never ran\n' ""
else
  printf '  %-52s ok    %s (the denominator)\n' "proof resends attempted" "$rb"
  note "  of those, REJECTED (result=error)" "$(v ha_miner_proof_rebroadcasts_total '{result="error"}')" 0
  note "  of those, window already closed" "$(v ha_miner_proof_rebroadcasts_total '{result="window_closed"}')" 0
fi
note "claims the chain did NOT have" "$(v ha_miner_claim_inclusion_outcome_total '{outcome!="on_chain_found"}')" 0
note "proofs the chain did NOT have" "$(v ha_miner_proof_inclusion_outcome_total '{outcome!="on_chain_found"}')" 0
echo "  (decoys, NOT loss: relays_lost=$(v ha_miner_relays_lost_total)"
echo "   sessions_failed=$(v ha_miner_sessions_failed_total) upokt_lost_uPOKT=$(vu ha_miner_upokt_lost_total) — see METRICS_TRIAGE.md section 1)"

# The ledger identity. A session leaves the book through exactly one door, so the
# doors must sum. Measured 2026-09-17 BEFORE this was enforced: claimed 6742.81,
# proved 4277.10, lost 3034.77 -> residual -569.06, i.e. 108.4%. A session whose
# first submission failed was counted lost at the error and proved again when the
# retry landed. Until that is fixed this line is expected to show a gap; once it
# is, a non-zero residual means a session vanished or was double counted.
echo
# The identity balances only once every session has left the book through a door,
# so it is a run-END reading. Mid-run it shows claimed > the sum of the doors for a
# structural reason and not a defect: a session inside its proof window has been
# claimed and has not reached any door yet. Measured 2026-09-18 during a gate run:
# 304 claims on chain, claimed 384000 uPOKT, every door still 0.
echo "1b. Does the money ledger close?  (a run-END reading: see the note in the source)"
cl=$(vu ha_miner_upokt_claimed_total); pr=$(vu ha_miner_upokt_proved_total)
lo=$(vu ha_miner_upokt_lost_total)
# Unresolved is a PAIR of counters, not a gauge, and the reason is the restart: a
# gauge resets with the process, so a session left unresolved by one replica and
# resolved by another reads wrong from either side. Two counters summed across
# instances give the right outstanding count across a restart, which is the exact
# scenario this system is built for.
uo=$(vu ha_miner_upokt_unresolved_opened_total)
ur=$(vu ha_miner_upokt_unresolved_resolved_total)
[ "$uo" = BAD-NAME ] && uo=0
[ "$ur" = BAD-NAME ] && ur=0
un=$((uo-ur))
if [ "$cl" = BAD-NAME ] || [ "$pr" = BAD-NAME ] || [ "$lo" = BAD-NAME ]; then
  printf '  %-52s BAD-NAME <- fix the script\n' "claimed == proved + lost + unresolved"
  failures=$((failures+1))
else
  [ "$un" = BAD-NAME ] && un=0
    if [ "$cl" -eq 0 ] && [ "$pr" -eq 0 ] && [ "$lo" -eq 0 ] && [ "$un" -eq 0 ]; then
      # Every door at zero. The identity would print OK and it would mean nothing:
      # there is no claimed amount to divide by. Say that instead of a pass.
      printf '  %-52s NO-DENOM  nothing claimed in this window, so the\n' "claimed == proved + lost + unresolved"
      printf '  %-52s           identity closing says nothing\n' ""
    else
      # Tolerance 0 on purpose. The terms accumulate one float division per
      # session, so a residual of a handful of uPOKT over billions would be float
      # accumulation and not a lost session -- but that has NOT been measured, so
      # it stays red until it is, with the number written down right here.
      identity "claimed == proved + lost + unresolved" "$cl" "$((pr+lo+un))"
    fi
  printf '  %-52s %s / %s / %s / %s  (uPOKT)\n' "  claimed / proved / lost / unresolved" "$cl" "$pr" "$lo" "$un"
fi

echo
echo "2. Was Redis the constraint, and did it cost anything?"
note "gate transitions"                    "$(v ha_transport_store_transitions_total)" 0
note "seconds closed (summed over series)" "$(v ha_transport_store_closed_seconds_total)" 0
note "relays the relayer refused"          "$(v ha_relayer_relays_rejected_total)" 0
note "SMST store write errors"             "$(v ha_smst_store_errors_total)" 0
note "audit writes that FAILED"             "$(v ha_miner_tracking_writes_failed_total)" 0
note "outcomes with no record to annotate"   "$(v ha_miner_tracking_outcomes_without_record_total)" 0
echo "   (the second one means a claim was submitted and its record never landed:"
echo "    its on-chain outcome is lost for good, see section 2)"

echo
echo "3. Was memory the constraint?"
note "the ingestion brake closed"          "$(v ha_miner_ingestion_memory_brake_transitions_total)" 0
note "trees unloaded when sessions ended"  "$(v ha_miner_smst_trees_unloaded_total)" -1
note "proofs that waited for memory"       "$(v ha_smst_rebuild_waiting)" 0
note "the GC CPU limiter engaged"          "$(v go_gc_limiter_last_enabled_gc_cycle)" 0

echo
echo "4. Did relays reach the tree?"
srv=$(v ha_relayer_relays_served_total); pub=$(v ha_relayer_relays_published_total)
skp=$(v ha_relayer_relays_skipped_difficulty_total)
con=$(v ha_miner_relays_consumed_from_stream_total); add=$(v ha_miner_relays_added_to_smst_total)
identity "served == published + skipped by difficulty" "$srv" "$((pub+skp))" 1000
identity "consumed from stream == added to the tree"   "$con" "$add"

echo
echo "5. Transactions"
note "rejections the chain returned on broadcast" "$(v ha_tx_broadcast_rejections_total)" 0
note "broadcast permits saturated"                "$(v ha_tx_permit_saturated_total)" 0
echo "   (a rejection returned by SIMULATION has no series: see 'What has NO series at all')"

echo
if [ "$failures" -eq 0 ]; then
  echo "TRIAGE=OK  every identity closes"
else
  echo "TRIAGE=REVIEW  $failures identity(ies) did not close"
fi
# The artefact carries its own exit code: a completion notice reports the exit of
# the last command in a pipeline, not this script's.
echo "EXIT=$failures"
exit 0
