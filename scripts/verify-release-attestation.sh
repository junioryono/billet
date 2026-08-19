#!/bin/sh
set -eu

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${RELEASE_TAG:?RELEASE_TAG is required}"

attempts=${RELEASE_VERIFY_ATTEMPTS:-12}
interval=${RELEASE_VERIFY_INTERVAL_SECONDS:-5}
deadline_seconds=${RELEASE_VERIFY_DEADLINE_SECONDS:-75}
attempt_timeout=${RELEASE_VERIFY_ATTEMPT_TIMEOUT_SECONDS:-15}
case "$attempts" in
    ''|*[!0-9]*|0) printf 'RELEASE_VERIFY_ATTEMPTS must be a positive integer\n' >&2; exit 2 ;;
esac
case "$interval" in
    ''|*[!0-9]*) printf 'RELEASE_VERIFY_INTERVAL_SECONDS must be a non-negative integer\n' >&2; exit 2 ;;
esac
case "$deadline_seconds" in
    ''|*[!0-9]*|0) printf 'RELEASE_VERIFY_DEADLINE_SECONDS must be a positive integer\n' >&2; exit 2 ;;
esac
case "$attempt_timeout" in
    ''|*[!0-9]*|0) printf 'RELEASE_VERIFY_ATTEMPT_TIMEOUT_SECONDS must be a positive integer\n' >&2; exit 2 ;;
esac

deadline=$(( $(date +%s) + deadline_seconds ))

run_before_deadline() {
    remaining=$((deadline - $(date +%s)))
    if [ "$remaining" -le 0 ]; then
        printf 'release attestation verification exceeded its %s-second deadline\n' "$deadline_seconds" >&2
        return 124
    fi
    command_timeout=$attempt_timeout
    if [ "$remaining" -lt "$command_timeout" ]; then
        command_timeout=$remaining
    fi
    timeout --signal=KILL "${command_timeout}s" "$@"
}

if immutable=$(run_before_deadline gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --json isImmutable --jq .isImmutable 2>&1); then
    :
else
    command_status=$?
    printf '%s\n' "$immutable" >&2
    exit "$command_status"
fi
if [ "$immutable" != true ]; then
    printf 'release %s is not immutable\n' "$RELEASE_TAG" >&2
    exit 1
fi

if object_format=$(run_before_deadline git rev-parse --show-object-format 2>&1); then
    :
else
    command_status=$?
    printf '%s\n' "$object_format" >&2
    exit "$command_status"
fi
case "$object_format" in
    sha1|sha256) ;;
    *) printf 'unsupported Git object format: %s\n' "$object_format" >&2; exit 1 ;;
esac
if tag_oid=$(run_before_deadline git rev-parse "refs/tags/${RELEASE_TAG}" 2>&1); then
    :
else
    command_status=$?
    printf '%s\n' "$tag_oid" >&2
    exit "$command_status"
fi
case "$tag_oid" in
    ''|*[!0-9a-f]*) printf 'invalid tag object ID: %s\n' "$tag_oid" >&2; exit 1 ;;
esac
case "$object_format:${#tag_oid}" in
    sha1:40|sha256:64) ;;
    *) printf 'tag object ID length does not match %s\n' "$object_format" >&2; exit 1 ;;
esac

endpoint="repos/${GITHUB_REPOSITORY}/attestations/${object_format}:${tag_oid}?predicate_type=release&per_page=100"
attempt=1
while :; do
    pending=false
    if response=$(run_before_deadline gh api --paginate --slurp "$endpoint" 2>&1); then
        if bundle_urls=$(printf '%s\n' "$response" | jq -r '
            def github_attestations:
                .[] | .attestations[] | select(.initiator == "github");
            if type != "array" or length == 0 or
                any(.[]; type != "object" or (.attestations | type) != "array") or
                any(.[] | .attestations[];
                    type != "object" or (.initiator | type) != "string" or (.initiator | length) == 0)
            then
                error("malformed paginated attestation response")
            elif any(github_attestations; (.bundle_url | type) != "string" or (.bundle_url | length) == 0) then
                error("GitHub release attestation has no bundle_url")
            else
                [github_attestations | .bundle_url][]
            end' 2>&1); then
            :
        else
            command_status=$?
            printf '%s\n' "$bundle_urls" >&2
            exit "$command_status"
        fi
        if [ -z "$bundle_urls" ]; then
            pending=true
        fi
    else
        command_status=$?
        http_status=$(printf '%s\n' "$response" | sed -n 's/^gh: .* (HTTP \([0-9][0-9][0-9]\))$/\1/p' | tail -n 1)
        if [ "$http_status" = 404 ]; then
            pending=true
        else
            printf '%s\n' "$response" >&2
            exit "$command_status"
        fi
    fi
    if [ "$pending" = false ]; then
        if verified=$(run_before_deadline gh release verify "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --format json 2>&1); then
            if attestation_tag=$(printf '%s\n' "$verified" | jq -er '.verificationResult.statement.predicate.tag' 2>&1); then
                :
            else
                command_status=$?
                printf '%s\n' "$attestation_tag" >&2
                exit "$command_status"
            fi
            if [ "$attestation_tag" != "$RELEASE_TAG" ]; then
                printf 'verified release attestation names %s, want %s\n' "$attestation_tag" "$RELEASE_TAG" >&2
                exit 1
            fi
            break
        else
            command_status=$?
            case "$verified" in
                *"no attestations found for release ${RELEASE_TAG} in "*) pending=true ;;
                *) printf '%s\n' "$verified" >&2; exit "$command_status" ;;
            esac
        fi
    fi
    [ "$pending" = true ] || exit 1
    if [ "$attempt" -ge "$attempts" ]; then
        printf 'release attestation was not visible after %s attempts\n' "$attempts" >&2
        exit 1
    fi

    printf 'release attestation is not visible yet; retrying (%s/%s)\n' "$attempt" "$attempts" >&2
    remaining=$((deadline - $(date +%s)))
    if [ "$remaining" -le 0 ]; then
        printf 'release attestation verification exceeded its %s-second deadline\n' "$deadline_seconds" >&2
        exit 124
    fi
    sleep_for=$interval
    if [ "$remaining" -lt "$sleep_for" ]; then
        sleep_for=$remaining
    fi
    sleep "$sleep_for"
    attempt=$((attempt + 1))
done

printf 'verified release attestation for %s\n' "$RELEASE_TAG"
