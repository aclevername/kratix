#!/usr/bin/env bash

# simulate-requests.sh
#
# Simulates a busy platform so you can watch the Kratix metrics move in
# Prometheus/Grafana. It runs two independent streams of activity against the
# kafka and redis resource requests in the cluster:
#
#   * Mutations  - every 5-15s, flip spec.size (small <-> large) on 1-3 requests.
#   * Churn      - every 10-20s, create brand new requests and delete old ones,
#                  keeping the population bobbing around a target range, with the
#                  occasional burst to mimic a busy period.
#
# Each create, delete and size change triggers a Kratix workflow, which is what
# drives the metrics. Workflow failures are NOT simulated here on purpose: the
# promises are already wired to fail ~1 in 5-10 runs, so a healthy mix of
# success/failure and retries will show up in the metrics on its own.
#
# Only simulator-created requests (labelled simulate-requests=true) are ever
# deleted, so your original baseline requests from the yaml files stay put and
# the platform never fully empties.
#
# Usage:
#   ./simulate-requests.sh            # run the simulation until Ctrl+C
#   ./simulate-requests.sh cleanup    # delete everything the simulator created
#
# Environment overrides:
#   NAMESPACE       namespace the requests live in     (default: default)
#   CONTEXT         kubectl context to target          (default: current context)
#   KINDS           space-separated resource kinds     (default: "kafka redis")
#   API_VERSION     apiVersion for created requests    (default: marketplace.kratix.io/v1alpha1)
#   MIN_REQUESTS    lower bound for population churn    (default: 5)
#   MAX_REQUESTS    upper bound for population churn    (default: 15)

# We deliberately avoid `set -e`: this is a long-running loop and a transient
# kubectl hiccup should not kill the simulation.
set -uo pipefail

NAMESPACE="${NAMESPACE:-default}"
CONTEXT="${CONTEXT:-}"
read -r -a KINDS <<< "${KINDS:-kafka redis}"
API_VERSION="${API_VERSION:-marketplace.kratix.io/v1alpha1}"

MIN_INTERVAL=5           # seconds between mutation batches (lower bound)
MAX_INTERVAL=15          # seconds between mutation batches (upper bound)
MAX_MUTATIONS=3          # up to this many requests mutated per batch
MAX_STAGGER=3            # max seconds of delay between staggered mutations

CHURN_MIN_INTERVAL=10    # seconds between churn events (lower bound)
CHURN_MAX_INTERVAL=20    # seconds between churn events (upper bound)
MIN_REQUESTS="${MIN_REQUESTS:-5}"
MAX_REQUESTS="${MAX_REQUESTS:-15}"

SIM_LABEL="simulate-requests=true"

KINDS_CSV=$(IFS=,; echo "${KINDS[*]}")
kubectl_args=(--namespace "$NAMESPACE")
if [ -n "$CONTEXT" ]; then
    kubectl_args+=(--context "$CONTEXT")
fi

timestamp() {
    date "+%H:%M:%S"
}

# random_int returns a random integer in the inclusive range [$1, $2].
random_int() {
    local min=$1 max=$2
    echo $(( RANDOM % (max - min + 1) + min ))
}

# list_requests prints every request (all kinds) as "<resource>.<group>/<name>".
list_requests() {
    kubectl "${kubectl_args[@]}" get "$KINDS_CSV" -o name 2>/dev/null
}

# count_requests prints the current number of requests across all kinds.
count_requests() {
    local n
    n=$(list_requests | wc -l)
    echo $(( n ))
}

# toggle_size flips a single request between small and large.
toggle_size() {
    local resource=$1
    local current new
    current=$(kubectl "${kubectl_args[@]}" get "$resource" -o jsonpath='{.spec.size}' 2>/dev/null)

    if [ "$current" = "large" ]; then
        new="small"
    else
        new="large"
    fi

    if kubectl "${kubectl_args[@]}" patch "$resource" --type=merge \
        -p "{\"spec\":{\"size\":\"$new\"}}" >/dev/null 2>&1; then
        echo "[$(timestamp)] mutate  $resource: ${current:-unset} -> $new"
    else
        echo "[$(timestamp)] mutate  $resource: failed (does it still exist?)"
    fi
}

# create_request creates a new random request labelled as simulator-owned.
create_request() {
    local kind size name
    kind=${KINDS[$(( RANDOM % ${#KINDS[@]} ))]}
    if (( RANDOM % 2 == 0 )); then size="small"; else size="large"; fi
    name="sim-${kind}-$(( RANDOM % 100000 ))"

    if kubectl "${kubectl_args[@]}" apply -f - >/dev/null 2>&1 <<EOF
apiVersion: ${API_VERSION}
kind: ${kind}
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    simulate-requests: "true"
spec:
  size: ${size}
EOF
    then
        echo "[$(timestamp)] create  ${kind}/${name} (size: ${size})"
    else
        echo "[$(timestamp)] create  ${kind}/${name}: failed"
    fi
}

# delete_request deletes the oldest simulator-created request, if any exist.
delete_request() {
    local resource
    resource=$(kubectl "${kubectl_args[@]}" get "$KINDS_CSV" -l "$SIM_LABEL" \
        --sort-by=.metadata.creationTimestamp -o name 2>/dev/null | head -n 1)

    if [ -z "$resource" ]; then
        return 0
    fi

    if kubectl "${kubectl_args[@]}" delete "$resource" --wait=false >/dev/null 2>&1; then
        echo "[$(timestamp)] delete  $resource"
    else
        echo "[$(timestamp)] delete  $resource: failed"
    fi
}

# pick_random prints $1 distinct, randomly chosen entries from the remaining args.
pick_random() {
    local count=$1; shift
    local pool=("$@")
    if (( count > ${#pool[@]} )); then
        count=${#pool[@]}
    fi

    local i idx
    for (( i = 0; i < count; i++ )); do
        idx=$(( RANDOM % ${#pool[@]} ))
        echo "${pool[$idx]}"
        # Remove the chosen entry so we do not pick it twice in this batch.
        pool=("${pool[@]:0:idx}" "${pool[@]:idx+1}")
    done
}

# do_mutation_batch toggles the size of 1-3 randomly chosen live requests,
# sometimes staggered by a second or two.
do_mutation_batch() {
    local requests=() line
    while IFS= read -r line; do
        if [ -n "$line" ]; then requests+=("$line"); fi
    done < <(list_requests)

    if [ ${#requests[@]} -eq 0 ]; then
        return 0
    fi

    local batch_size staggered resource
    batch_size=$(random_int 1 "$MAX_MUTATIONS")
    staggered=$(( RANDOM % 2 ))

    while IFS= read -r resource; do
        toggle_size "$resource"
        if [ "$staggered" -eq 1 ]; then
            sleep "$(random_int 1 "$MAX_STAGGER")"
        fi
    done < <(pick_random "$batch_size" "${requests[@]}")
}

# do_churn creates and/or deletes requests, keeping the population within
# [MIN_REQUESTS, MAX_REQUESTS] while letting it wander for a lively feel.
do_churn() {
    local count
    count=$(count_requests)

    if (( count <= MIN_REQUESTS )); then
        create_request
        return 0
    fi

    if (( count >= MAX_REQUESTS )); then
        delete_request
        return 0
    fi

    # In the healthy middle: usually create, sometimes delete, sometimes both
    # (turnover), and occasionally a burst of creates to mimic a busy period.
    local roll n i
    roll=$(( RANDOM % 10 ))
    if (( roll < 2 )); then
        n=$(random_int 2 4)
        echo "[$(timestamp)] burst   creating $n requests"
        for (( i = 0; i < n; i++ )); do create_request; done
    elif (( roll < 6 )); then
        create_request
    elif (( roll < 8 )); then
        delete_request
    else
        create_request
        delete_request
    fi
}

# cleanup deletes every request the simulator ever created and exits.
cleanup() {
    echo "Deleting all simulator-created requests..."
    local resource found=false
    while IFS= read -r resource; do
        if [ -n "$resource" ]; then
            found=true
            kubectl "${kubectl_args[@]}" delete "$resource" --wait=false
        fi
    done < <(kubectl "${kubectl_args[@]}" get "$KINDS_CSV" -l "$SIM_LABEL" -o name 2>/dev/null)

    if [ "$found" = false ]; then
        echo "Nothing to clean up."
    fi
}

main() {
    if [ "${1:-}" = "cleanup" ] || [ "${1:-}" = "--cleanup" ]; then
        cleanup
        exit 0
    fi

    trap 'echo; echo "Stopping simulation."; exit 0' INT TERM

    if [ "$(count_requests)" -eq 0 ]; then
        echo "No requests found for kinds: ${KINDS[*]} (namespace: $NAMESPACE)."
        echo "Have you applied kafka-requests.yaml and redis-requests.yaml?"
        exit 1
    fi

    echo "Simulating a busy platform. Press Ctrl+C to stop."
    echo "  - mutating sizes every ${MIN_INTERVAL}-${MAX_INTERVAL}s (1-${MAX_MUTATIONS} at a time)"
    echo "  - creating/deleting requests every ${CHURN_MIN_INTERVAL}-${CHURN_MAX_INTERVAL}s"
    echo "  - keeping roughly ${MIN_REQUESTS}-${MAX_REQUESTS} requests alive"
    echo

    local now next_mutate next_churn target sleep_for
    next_mutate=$(( SECONDS + $(random_int "$MIN_INTERVAL" "$MAX_INTERVAL") ))
    next_churn=$(( SECONDS + $(random_int "$CHURN_MIN_INTERVAL" "$CHURN_MAX_INTERVAL") ))

    while true; do
        now=$SECONDS

        if (( now >= next_mutate )); then
            do_mutation_batch
            next_mutate=$(( SECONDS + $(random_int "$MIN_INTERVAL" "$MAX_INTERVAL") ))
        fi

        if (( now >= next_churn )); then
            do_churn
            next_churn=$(( SECONDS + $(random_int "$CHURN_MIN_INTERVAL" "$CHURN_MAX_INTERVAL") ))
        fi

        # Sleep until whichever stream is due next (at least a second).
        target=$next_mutate
        if (( next_churn < target )); then
            target=$next_churn
        fi
        sleep_for=$(( target - SECONDS ))
        if (( sleep_for < 1 )); then
            sleep_for=1
        fi
        sleep "$sleep_for"
    done
}

main "$@"
