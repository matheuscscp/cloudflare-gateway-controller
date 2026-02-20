#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# Generates the Helm chart CRDs from the controller-gen
# output at config/crd/bases/.  Run via "make manifests".

set -euo pipefail

CRD_DIR=config/crd/bases
CHART_CRD=charts/cloudflare-gateway-controller/templates/crds.yaml

cat > "$CHART_CRD" <<'HEADER'
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

HEADER

for f in "$CRD_DIR"/*.yaml; do
  cat "$f" >> "$CHART_CRD"
done
