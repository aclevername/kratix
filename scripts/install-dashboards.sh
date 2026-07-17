#!/usr/bin/env bash

# install-dashboards.sh
#
# Loads the Kratix Grafana dashboards (hack/platform/dashboards/*.json) into the
# cluster as ConfigMaps. The Grafana that ships with kube-prometheus-stack runs a
# sidecar that watches for ConfigMaps labelled `grafana_dashboard=1` and imports
# them automatically, so there is nothing to click in the Grafana UI.
#
# Re-running is safe: the ConfigMaps are applied (created or updated) each time.
#
# Usage:
#   ./scripts/install-dashboards.sh
#
# Environment overrides:
#   CONTEXT               kubectl context to target (default: kind-platform)
#   MONITORING_NAMESPACE  namespace Grafana runs in (default: monitoring)

set -uo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." &> /dev/null && pwd)
CONTEXT="${CONTEXT:-kind-platform}"
MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
DASHBOARD_DIR="${ROOT}/hack/platform/dashboards"

for dashboard in "${DASHBOARD_DIR}"/*.json; do
    base=$(basename "${dashboard}")
    configmap="kratix-dashboard-${base%.json}"

    kubectl --context "${CONTEXT}" -n "${MONITORING_NAMESPACE}" create configmap "${configmap}" \
        --from-file="${base}=${dashboard}" \
        --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -

    # The sidecar only imports ConfigMaps carrying this label.
    kubectl --context "${CONTEXT}" -n "${MONITORING_NAMESPACE}" label configmap "${configmap}" \
        grafana_dashboard=1 --overwrite >/dev/null

    echo "loaded dashboard ${base} as configmap ${configmap}"
done
