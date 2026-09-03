#!/bin/sh
# refuse-active-builds.sh — the destroy-time guard for a billet CodeBuild fleet.
#
# `terraform destroy` on this module removes the node role, the build role and the log
# group that a RUNNING build depends on, and AWS will not stop it: DeleteProject
# succeeds while a build is in progress and the build carries on to completion
# (measured 2026-09-02, docs/reference/records/aws-acceptance.md). GitHub does not requeue a job whose
# runner vanished mid-execution, so a destroy under a live build is somebody's failed
# build. This script runs from a destroy-time provisioner BEFORE any of that is removed
# and exits non-zero while anything in the project could still be running.
#
# WHAT IT NEEDS: the AWS CLI on PATH and credentials that may ListBuildsForProject and
# BatchGetBuilds on the project (the operator's, not the node's). BILLET_GUARD_PROJECT
# and BILLET_GUARD_REGION arrive from the provisioner.
#
# THE ESCAPE IS AN ENVIRONMENT VARIABLE, NOT A MODULE VARIABLE. A destroy-time
# provisioner sees the values in state rather than on the command line, so a -var at
# destroy time would never reach it and a module variable would need its own apply
# first. `BILLET_SKIP_ACTIVE_BUILD_GUARD=1 terraform destroy` is the operator asserting
# the fleet was drained with `billet drain --wait`.
#
# THE WINDOW IS THE SERVICE'S CEILINGS, NOT THE MODULE'S. A build cannot still be running
# once CodeBuild's own maximum build and queued timeouts have both elapsed, so the walk
# stops there whatever timeouts the project was configured with — an adopted project
# keeps timeouts the module never set. The abandon cutoff is one queued ceiling older
# than the live cutoff, for the reason internal/provider/codebuild's walk gives: the
# listing is ordered by submission, and a build that queued for hours STARTED later
# than one submitted after it.
#
# "COULD NOT TELL" IS NEVER "NOTHING RUNNING". A CLI error, an id CodeBuild did not know,
# a status this script has never heard of, a pagination token that repeats, a walk
# that never reaches the cutoff, and an answer that is not the shape the CLI was asked
# for — a listing that is not one three-field line, a batch that omits, repeats or
# invents an id, a start time that does not read as a timestamp — all refuse. Only a
# walk that saw every build inside the window and found each one terminal lets the
# destroy proceed.
#
# POSIX sh. No pipefail, no `!` before a pipeline, no `;`-chained gates: every
# refusal is an `if` with a message and an explicit exit, because a gate in the middle
# of a chain is not a gate.

set -eu

# THE CLI RENDERS TIMESTAMPS IN THE MACHINE'S LOCAL ZONE, measured: on a Pacific
# laptop `startTime` came back as 2026-09-01T18:37:02.895000-07:00, which compared
# lexically against a UTC cutoff is seven hours wrong. Under TZ=UTC the same field is
# 2026-09-02T01:37:02.895000+00:00, and the cutoffs below are computed in UTC too.
TZ=UTC
export TZ

# AND THE COLLATION IS BYTES: sort(1) orders by the locale's collation, and the
# timestamp comparison below is only chronological when it is byte order.
LC_ALL=C
export LC_ALL

: "${BILLET_GUARD_PROJECT:?BILLET_GUARD_PROJECT is required}"
: "${BILLET_GUARD_REGION:?BILLET_GUARD_REGION is required}"

# CodeBuild's service ceilings, in minutes: 2160 build, 480 queued, plus the same hour
# of slack billet's inventory walk uses. The abandon cutoff subtracts the queued
# ceiling again.
live_window_minutes=$((2160 + 480 + 60))
abandon_window_minutes=$((live_window_minutes + 480))
max_pages=500

refuse() {
  echo "refuse-active-builds: $1" >&2
  echo "refuse-active-builds: drain first with \`billet drain --wait\` on the control plane, or set BILLET_SKIP_ACTIVE_BUILD_GUARD=1 to assert the fleet is drained" >&2
  exit 1
}

if [ "${BILLET_SKIP_ACTIVE_BUILD_GUARD:-}" = "1" ]; then
  echo "refuse-active-builds: BILLET_SKIP_ACTIVE_BUILD_GUARD=1 — proceeding on the operator's assertion that project $BILLET_GUARD_PROJECT has no running build; nothing was checked"
  exit 0
fi

if command -v aws >/dev/null 2>&1; then
  :
else
  refuse "the aws CLI is not on PATH, so whether project $BILLET_GUARD_PROJECT has a running build cannot be established"
fi

# cutoff_iso prints an ISO-8601 UTC timestamp (to the second) N minutes in the past.
# GNU date spells "an epoch" as -d @N and BSD date as -r N; both are tried, because the
# operator's machine may be either and a guard that cannot compute its cutoff must
# refuse rather than guess.
cutoff_iso() {
  epoch=$(( $(date +%s) - $1 * 60 ))
  if out=$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%S 2>/dev/null); then
    echo "$out"
    return 0
  fi
  if out=$(date -u -r "$epoch" +%Y-%m-%dT%H:%M:%S 2>/dev/null); then
    echo "$out"
    return 0
  fi
  return 1
}

if live_cutoff=$(cutoff_iso "$live_window_minutes"); then
  :
else
  refuse "this machine's date(1) understands neither -d @epoch nor -r epoch, so the inventory window cannot be computed"
fi
abandon_cutoff=$(cutoff_iso "$abandon_window_minutes")
abandon_epoch=$(( $(date +%s) - abandon_window_minutes * 60 ))

# older_than answers whether an AWS startTime (YYYY-MM-DDTHH:MM:SS.ffffff+00:00, always
# UTC from the CLI) is before a cutoff, comparing the first 19 characters lexically —
# valid because both are UTC in one fixed layout. A build with no start time ("None":
# SUBMITTED or QUEUED) is inside the window, deliberately: it is the build most likely
# to be about to run somebody's job.
older_than() {
  case "$1" in
    None|"") return 1 ;;
  esac
  # The wire rendering is epoch seconds, compared numerically against the epoch
  # cutoff (the integer part is enough: the cutoff is minutes wide).
  if is_wire "$1"; then
    if [ "${1%%.*}" -lt "$3" ]; then
      return 0
    fi
    return 1
  fi
  head19=$(printf '%s' "$1" | cut -c1-19)
  if [ "$head19" = "$2" ]; then
    return 1
  fi
  # POSIX test(1) has no string ordering, so the smaller of the two is whichever
  # sort(1) puts first.
  first=$(printf '%s\n%s\n' "$head19" "$2" | sort | head -n 1)
  if [ "$first" = "$head19" ]; then
    return 0
  fi
  return 1
}

tab=$(printf '\t')
nl='
'
# timestamp_ok answers whether a start time is one this script can compare: None,
# the CLI's ISO rendering (YYYY-MM-DDTHH:MM:SS, an optional .ffffff, and +00:00 —
# the default in CLI v2), or its WIRE rendering (epoch seconds with an optional
# fraction — CLI v1's default, and v2 under cli_timestamp_format = wire, which
# TZ=UTC does not override). Each calendar field is checked against its range and
# the day against its month, because an impossible date compares as ancient.
timestamp_ok() {
  case "$1" in
    None) return 0 ;;
    [12][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]+00:00) ;;
    [12][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9]+00:00) ;;
    *)
      # Not ISO: it is the wire rendering only if it is digits with at most one
      # dot and a digit on either side of it.
      case "$1" in
        *[!0-9.]*|.*|*.|*.*.*) return 1 ;;
        *) return 0 ;;
      esac
      ;;
  esac
  year=$(printf '%s' "$1" | cut -c1-4)
  month=$(printf '%s' "$1" | cut -c6-7)
  day=$(printf '%s' "$1" | cut -c9-10)
  case "$month" in 0[1-9]|1[0-2]) ;; *) return 1 ;; esac
  case "$day" in 0[1-9]|[12][0-9]|3[01]) ;; *) return 1 ;; esac
  case "$(printf '%s' "$1" | cut -c12-13)" in [01][0-9]|2[0-3]) ;; *) return 1 ;; esac
  case "$(printf '%s' "$1" | cut -c15-16)" in [0-5][0-9]) ;; *) return 1 ;; esac
  case "$(printf '%s' "$1" | cut -c18-19)" in [0-5][0-9]) ;; *) return 1 ;; esac
  case "$month" in
    04|06|09|11) last=30 ;;
    02)
      # The year's first digit is 1 or 2 by the pattern above, so no leading zero can
      # make the arithmetic octal.
      y=$((year + 0))
      if [ $((y % 4)) -eq 0 ] && { [ $((y % 100)) -ne 0 ] || [ $((y % 400)) -eq 0 ]; }; then
        last=29
      else
        last=28
      fi
      ;;
    *) last=31 ;;
  esac
  if [ "$((${day#0} + 0))" -gt "$last" ]; then
    return 1
  fi
  return 0
}

# is_wire answers whether a start time is in the epoch rendering.
is_wire() {
  case "$1" in
    *[!0-9.]*) return 1 ;;
    *) return 0 ;;
  esac
}

running=""
token=""
seen_tokens=" "
page=0

while :; do
  page=$((page + 1))
  if [ "$page" -gt "$max_pages" ]; then
    refuse "walked $max_pages pages of project $BILLET_GUARD_PROJECT without reaching the inventory window's edge, so whether a build is running could not be established"
  fi

  if [ -n "$token" ]; then
    listing=$(aws codebuild list-builds-for-project --region "$BILLET_GUARD_REGION" \
      --project-name "$BILLET_GUARD_PROJECT" --next-token "$token" --no-paginate \
      --query '[join(`,`, ids), nextToken == `null`, nextToken]' --output text 2>&1) || refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT failed: $listing"
  else
    listing=$(aws codebuild list-builds-for-project --region "$BILLET_GUARD_REGION" \
      --project-name "$BILLET_GUARD_PROJECT" --no-paginate \
      --query '[join(`,`, ids), nextToken == `null`, nextToken]' --output text 2>&1) || refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT failed: $listing"
  fi

  # THREE FIELDS come back tab-separated on ONE line: the ids joined by commas (an
  # empty list is an empty field), whether the token is ABSENT — `nextToken == null`,
  # which the CLI renders as True or False (measured) — and the token, or None. The
  # presence flag exists because a token is opaque and nothing rules out one that
  # spells "None"; reading it as absent would end the walk over the page it names.
  # Anything else — no line, several lines, the wrong number of fields, a flag that
  # is neither True nor False, an id list with an empty component — is an answer
  # this script did not ask for.
  case "$listing" in
    *"$nl"*) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT answered in a shape this script cannot read: $listing" ;;
    *"$tab"*"$tab"*) ;;
    *) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT answered with fewer fields than were asked for: $listing" ;;
  esac
  first=${listing%%"$tab"*}
  rest=${listing#*"$tab"}
  absent=${rest%%"$tab"*}
  next=${rest#*"$tab"}
  case "$next" in
    *"$tab"*) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT answered with more fields than were asked for: $listing" ;;
  esac
  case "$absent" in
    True)
      if [ "$next" != "None" ]; then
        refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT reported no token and then a token: $listing"
      fi
      next=""
      ;;
    False)
      if [ -z "$next" ]; then
        refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT reported a token and then none: $listing"
      fi
      ;;
    *) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT answered in a shape this script cannot read: $listing" ;;
  esac
  case "$first" in
    "") ;;
    ,*|*,|*,,*|*" "*|*"$tab"*) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT listed an id this script cannot read: $listing" ;;
  esac
  ids=$(printf '%s' "$first" | tr ',' ' ')

  if [ -n "$next" ]; then
    case "$seen_tokens" in
      *" $next "*) refuse "ListBuildsForProject on $BILLET_GUARD_PROJECT handed back a pagination token it already issued, so the listing cannot be trusted to end" ;;
    esac
    seen_tokens="$seen_tokens$next "
  fi

  # An empty page with a token is a page to walk past; an empty page without one is
  # the end of the listing.
  if [ -z "$(printf '%s' "$ids" | tr -d ' ')" ]; then
    if [ -z "$next" ]; then
      break
    fi
    token=$next
    continue
  fi

  # shellcheck disable=SC2086 # the ids are deliberately word-split into arguments
  batch=$(aws codebuild batch-get-builds --region "$BILLET_GUARD_REGION" --ids $ids \
    --query '[builds[].[id, buildStatus, startTime], buildsNotFound]' --output text 2>&1) || refuse "BatchGetBuilds on $BILLET_GUARD_PROJECT failed: $batch"

  # Text output renders each build as one tab-separated line, and the not-found ids on
  # lines of their own without tabs (measured; an empty not-found list prints nothing).
  # A not-found id is a build this script cannot judge. AND EVERY ID ASKED ABOUT MUST
  # COME BACK EXACTLY ONCE, in one list or the other: a batch that silently omits an
  # id, answers twice about one, or answers about one that was never asked for is not
  # the answer to this question, and an omitted build could be the one running.
  expected=0
  for id in $ids; do
    expected=$((expected + 1))
  done
  seen=0
  seen_ids=" "
  all_old=1
  while IFS="$tab" read -r id status start extra; do
    if [ -z "$id" ]; then
      continue
    fi
    if [ -n "$extra" ]; then
      refuse "BatchGetBuilds answered about build $id with more fields than were asked for"
    fi
    case " $ids " in
      *" $id "*) ;;
      *) refuse "BatchGetBuilds answered about build $id, which this script did not ask about" ;;
    esac
    case "$seen_ids" in
      *" $id "*) refuse "BatchGetBuilds answered twice about build $id" ;;
    esac
    seen_ids="$seen_ids$id "
    seen=$((seen + 1))
    if [ -z "$status" ]; then
      refuse "BatchGetBuilds did not know build $id, so whether it is running could not be established"
    fi
    case "$status" in
      SUCCEEDED|FAILED|FAULT|TIMED_OUT|STOPPED) ;;
      *) running="$running $id($status)" ;;
    esac
    # A start time is "None" or EXACTLY the CLI's UTC rendering, measured as
    # 2026-09-02T01:37:02.895000+00:00 under TZ=UTC — or the same without the
    # fraction, which the CLI's ISO renderer omits when the microseconds are zero.
    # The whole value is matched, not a prefix, and every field is range-checked: a
    # value with a plausible shape and an impossible calendar (month 00, hour 29)
    # would compare as ancient and abandon a page holding a running build, and a
    # suffix other than +00:00 means the comparison against a UTC cutoff is wrong.
    if timestamp_ok "$start"; then
      :
    else
      refuse "BatchGetBuilds reported a start time for $id this script cannot read: $start"
    fi
    if older_than "$start" "$abandon_cutoff" "$abandon_epoch"; then
      :
    else
      all_old=0
    fi
  done <<EOF
$batch
EOF

  if [ "$seen" -ne "$expected" ]; then
    refuse "BatchGetBuilds answered about $seen of the $expected builds this script asked for, so whether the rest are running could not be established"
  fi

  if [ -n "$running" ]; then
    refuse "project $BILLET_GUARD_PROJECT still has a build that is not terminal:$running"
  fi

  if [ "$all_old" -eq 1 ]; then
    break
  fi

  if [ -z "$next" ]; then
    break
  fi
  token=$next
done

echo "refuse-active-builds: no build in project $BILLET_GUARD_PROJECT started since $live_cutoff UTC is still running ($page page(s) examined); destroy may proceed"
exit 0
