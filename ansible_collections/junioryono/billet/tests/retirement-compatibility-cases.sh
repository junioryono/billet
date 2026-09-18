#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside its selected CI section.
# R8: incapable or absent answerers without a candidate hold, independent of policy,
# requested retirement, installed node-only bytes or a fresh-host shape.
# v0.10.1 is explicitly below the complete-classifier floor, v0.11.0.
# Check mode reports the same hold and succeeds, without ordinary work.
for kind in below-floor unreadable absent; do
  for policy in true false; do
    for requested in false true; do
      for mode in normal check; do
        for shape in node fresh; do
          name=r8-$kind-$policy-$requested-$mode-$shape
          if [ "$kind" = below-floor ]; then r_plant "$name" v0.10.1; else r_plant "$name"; fi
          a "$name" -e "{\"billet_enable_server\": $policy}" -e "billet_server_retire=$requested"
          case "$kind" in
            unreadable) a "$name" -e '{"billet_gate_version":{"type":"unreadable"}}' ;;
            absent) p "$name" 'rm /usr/bin/billet' ;;
          esac
          case "$shape" in
            node) p "$name" 'mkdir -p /etc/billet; printf "node: {name: x}\n" >/etc/billet/billet.yaml' ;;
            fresh) p "$name" 'mkdir -p /var/lib/billet/server' ;;
          esac
          if [ "$mode" = check ]; then a "$name" --check; fi
          r_run "$name"
          if [ "$mode" = check ]; then
            expect_allowed "$name"
          else
            expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
          fi
          r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
          r_reported "$name" hold 'upgrade the managed binary or converge with an older collection'
          if [ "$requested" = true ]; then
            r_reported "$name" hold '--requested retirement is refused without a capable answerer'
          fi
          expect_no_ordinary "$name"
          expect_no_play_task "$name" 'Ordinary convergence sentinel'
          expect_host_commands "$name" ''
          expect_no_task "$name" 'Ask the retirement classifier'
          expect_no_task "$name" 'Inspect the transaction claim before recovery'
        done
      done
    done
  done
done

# An unknown route must traverse the caller's rescue and ordinary boundary,
# not only R7's parser harness. Corrupt a case-local copy, never the corpus.
r_plant r7-caller-unknown-route
"$python" - "$work/retire-fixtures/dry-run-ordinary.json" "$work/cases/r7-caller-unknown-route/unknown-route.json" <<'PYROUTE'
import json, sys
answer = json.load(open(sys.argv[1]))
answer['route'] = 'invented'
with open(sys.argv[2], 'w') as stream:
    json.dump(answer, stream)
PYROUTE
e r7-caller-unknown-route "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r7-caller-unknown-route"
r_answers r7-caller-unknown-route 'control-a:classify:1:unknown-route.json:0'
r_run r7-caller-unknown-route
r_held r7-caller-unknown-route hold 'Judge each retirement member'
expect_final r7-caller-unknown-route 'Retirement holds this host (hold)'
grep -qF 'answered with a member this role cannot read: route' "$work/cases/r7-caller-unknown-route/out" || fail 'r7-caller-unknown-route: the parser did not name the unknown route'
r_reported r7-caller-unknown-route hold

# The classifier already normalizes an unknown state into hold. Its reason
# must survive intact; the caller may not replace it with a state judgement.
r_plant r7-caller-unknown-state
"$python" - "$work/retire-fixtures/dry-run-hold-unreadable-row.json" "$work/cases/r7-caller-unknown-state/unknown-state.json" <<'PYSTATE'
import json, sys
answer = json.load(open(sys.argv[1]))
answer['state'] = 'unknown'
with open(sys.argv[2], 'w') as stream:
    json.dump(answer, stream)
PYSTATE
e r7-caller-unknown-state "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r7-caller-unknown-state"
r_answers r7-caller-unknown-state 'control-a:classify:1:unknown-state.json:0'
r_run r7-caller-unknown-state
r_held r7-caller-unknown-state hold
reason=$("$python" -c 'import json, sys; print(json.load(open(sys.argv[1]))["route_why"])' "$work/cases/r7-caller-unknown-state/unknown-state.json")
r_reported r7-caller-unknown-state hold "$reason"

# The missing/unknown version type cases enter the caller, not the JSON parser.
for spec in 'missing:{}' 'unknown:{"type":"invented"}' 'development:{"type":"development"}'; do
  name=r8-version-${spec%%:*}; value=${spec#*:}
  r_plant "$name"
  a "$name" -e "{\"billet_gate_version\":$value}"
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary.json:0'
  r_run "$name"
  if [ "${spec%%:*}" = development ]; then
    expect_allowed "$name"
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" 'control-a retire-classify 1;'
  else
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'This collection needs billet at or above v0.11.0 on the host'
    expect_no_ordinary "$name"
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" ''
  fi
done
r_plant r8-unanswered
r_answers r8-unanswered 'control-a:classify:1:dry-run-ordinary.json:0'
e r8-unanswered 'BILLET_GATE_DROP_ANSWER=retire-classify:1'
r_run r8-unanswered
[ "$status" -ne 0 ] || fail 'r8-unanswered: an unanswered classifier converged'
expect_final r8-unanswered 'did not answer retirement' 'state is unknown'
expect_host_commands r8-unanswered 'control-a retire-classify 1;'
expect_no_ordinary r8-unanswered
expect_no_play_task r8-unanswered 'Ordinary convergence sentinel'

# A development answerer attempts classification, including with --requested.
r_plant r8-requested-development
a r8-requested-development -e billet_server_retire=true
printf 'billet (devel) linux/amd64\n' >"$work/cases/r8-requested-development/version-line"
e r8-requested-development "BILLET_GATE_ANSWER=version:3:$work/cases/r8-requested-development/version-line"
r_answers r8-requested-development 'control-a:classify:1:dry-run-hold-unreadable-row.json:0'
r_run r8-requested-development
r_held r8-requested-development hold

# R9: every route in check mode, ordinary alone reaches its ordinary boundary.
for spec in ordinary:dry-run-ordinary hold:dry-run-hold-unreadable-row continue:dry-run-continue recovery:dry-run-recovery-guard new-request:dry-run-new-request cancel:dry-run-cancel unsupported-variant:dry-run-unsupported-variant; do
  route=${spec%%:*}; fixture=${spec#*:}; name=r9-$route
  r_plant "$name"
  if [ "$route" = new-request ]; then a "$name" -e billet_retirement_survivor_host=control-b; fi
  a "$name" --check
  r_answers "$name" "control-a:classify:1:$fixture.json:0"
  r_run "$name"
  expect_allowed "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
  r_reported "$name" "$route"
  if [ "$route" = ordinary ]; then expect_play_task_ran "$name" 'Ordinary convergence sentinel'; else expect_no_play_task "$name" 'Ordinary convergence sentinel'; fi
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# R9: the same fresh host previews without a classifier in check mode, and
# classifies through the candidate that real preparation stages in a real run.
for mode in check normal; do
  name=r9-fresh-candidate-$mode
  r_plant "$name"
  p "$name" 'rm /usr/bin/billet'
  a "$name" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary-never-commissioned.json:0'
  r_run "$name"
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  expect_state "$name" managed absent
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  if [ "$mode" = check ]; then
    r_reported "$name" unverified-check-mode 'The retirement route is unverified in check mode'
    r_reported "$name" unverified-check-mode 'A real run classifies with the staged candidate'
    r_reported "$name" unverified-check-mode 'Previewing ordinary tasks may differ from that classified route.'
    expect_host_commands "$name" ''
    expect_no_task "$name" 'Ask the retirement classifier'
    expect_no_task "$name" 'Stage the immutable candidate binary inside its recovery journal'
    expect_calls "$name" candidate '' 0
    expect_state "$name" active absent
  else
    r_reported "$name" ordinary
    expect_ran "$name" 'Stage the immutable candidate binary inside its recovery journal'
    expect_ran "$name" 'Ask the staged candidate to prepare'
    expect_host_commands "$name" 'control-a retire-classify 1;'
    expect_calls "$name" candidate 'server retire --dry-run' 1
    expect_state "$name" record_preparing False
  fi
done

# A newer candidate does not replace a managed answerer that carries the
# guard. The managed version remains below the retirement floor in both modes.
for mode in check normal; do
  name=r8-managed-with-candidate-$mode
  r_plant "$name" v0.10.1
  a "$name" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_run "$name"
  if [ "$mode" = check ]; then
    expect_allowed "$name"
    expect_no_task "$name" 'Stage the immutable candidate binary inside its recovery journal'
  else
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
    expect_ran "$name" 'Stage the immutable candidate binary inside its recovery journal'
  fi
  r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" ''
  expect_no_task "$name" 'Ask the retirement classifier'
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  expect_no_ordinary "$name"
done

# R8: the real entry refuses missing installation input BEFORE reporting any
# retirement route. Its empty config must not become the first refusal either.
for mode in check normal; do
  name=r8-missing-source-$mode
  r_plant "$name"
  p "$name" 'rm /usr/bin/billet'
  a "$name" -e billet_version= -e billet_release_channel=
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_run "$name" play-retirement-main
  expect_refused "$name" 'Validate the billet binary source before retirement routing' 'Name the binary with billet_binary_src'
  expect_no_task "$name" 'Report the retirement route'
  expect_no_task "$name" 'Ask the retirement classifier'
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  expect_host_commands "$name" ''
  expect_no_ordinary "$name"
done

# Legacy verification must discard preparation's non-empty dry-run answerer.
# The ordinary answer would open the sentinel if an unverified path survived.
for kind in symlink other-owner group-writable not-executable verified; do
  name=r9-legacy-$kind
  r_plant "$name"
  p "$name" 'printf "%s\n" "$ROOT/recovery-20260909T120000-12345678" >"$ROOT/active"'
  a "$name" --check -e "billet_gate_legacy_answerer=$kind" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary.json:0'
  r_run "$name"
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Prove legacy preparation published a dry-run answerer'
  expect_ran "$name" "Examine a legacy claim's read-only answerer"
  if [ "$kind" = verified ]; then
    expect_no_play_task "$name" 'Invalidate the legacy executable after preparation'
    expect_ran "$name" 'Select a verified legacy answerer'
    r_reported "$name" ordinary
    expect_host_commands "$name" 'control-a retire-classify 1;'
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  else
    expect_play_task_ran "$name" 'Invalidate the legacy executable after preparation'
    expect_no_task "$name" 'Select a verified legacy answerer'
    r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
    r_reported "$name" hold 'upgrade the managed binary or converge with an older collection'
    expect_host_commands "$name" ''
    expect_no_task "$name" 'Ask the retirement classifier'
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
  fi
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# An attempted but unanswered classification also reports hold in check mode.
r_plant r9-unanswered
a r9-unanswered --check
r_answers r9-unanswered 'control-a:classify:1:dry-run-ordinary.json:0'
e r9-unanswered 'BILLET_GATE_DROP_ANSWER=retire-classify:1'
r_run r9-unanswered
expect_allowed r9-unanswered
r_reported r9-unanswered hold 'did not answer retirement'
expect_no_ordinary r9-unanswered
expect_no_play_task r9-unanswered 'Ordinary convergence sentinel'
expect_no_task r9-unanswered 'Inspect the transaction claim before recovery'
expect_host_commands r9-unanswered 'control-a retire-classify 1;'

