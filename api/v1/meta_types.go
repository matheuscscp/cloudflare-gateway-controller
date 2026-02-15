// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1

// GatewayFinalizer is the finalizer added to Gateway resources to ensure
// the Cloudflare tunnel is deleted before the Gateway is removed.
const GatewayFinalizer = "gateway." + Group + "/finalizer"

// TunnelIDAnnotation is the annotation key used to store the Cloudflare
// tunnel ID on a Gateway resource.
const TunnelIDAnnotation = "gateway." + Group + "/tunnel-id"
