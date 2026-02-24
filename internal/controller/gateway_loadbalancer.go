// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// lbReconcileState captures the LB resource state produced by reconcileLoadBalancer,
// used to populate the CloudflareGatewayStatus.
type lbReconcileState struct {
	zoneID      string
	monitorID   string
	monitorName string
	pools       []apiv1.PoolStatus
	hostnames   []string
}

// reconcileLoadBalancer creates or updates the Cloudflare Load Balancer
// infrastructure: a health monitor, pools (per-AZ or per-Service depending
// on the topology), and one load balancer per hostname. Stale LBs and pools
// are cleaned up. previousHostnames is the list of LB hostnames from the
// previous reconciliation (from CGS.Status.LoadBalancer.Hostnames); stale
// hostnames are those in previousHostnames but not in the desired set.
// previousPools is the list of LB pools from the previous reconciliation
// (from CGS.Status.LoadBalancer.Pools); stale pools are those in
// previousPools but not in the desired set.
func (r *GatewayReconciler) reconcileLoadBalancer(
	ctx context.Context,
	tc cloudflare.Client,
	gw *gatewayv1.Gateway,
	params *apiv1.CloudflareGatewayParameters,
	mode lbMode,
	entries []tunnelEntry,
	validRoutes []*gatewayv1.HTTPRoute,
	previousHostnames []string,
	previousPools []apiv1.PoolStatus,
) (lbReconcileState, []string, []string) {
	l := log.FromContext(ctx)
	var state lbReconcileState
	var changes []string
	var errs []string

	zoneName := params.Spec.DNS.Zone.Name
	zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
	if err != nil {
		return state, nil, []string{fmt.Sprintf("failed to find zone ID for %q: %v", zoneName, err)}
	}
	state.zoneID = zoneID

	// 1. Ensure monitor.
	monitorName := apiv1.MonitorName(gw)
	monitorDesc := apiv1.ResourceDescription(gw)
	state.monitorName = monitorName
	resolved := apiv1.ResolveMonitorConfig(params.Spec.LoadBalancer)
	monitorCfg := cloudflare.MonitorConfig{
		Type:          resolved.Type,
		Path:          resolved.Path,
		Interval:      resolved.Interval,
		Timeout:       resolved.Timeout,
		ExpectedCodes: resolved.ExpectedCodes,
	}
	monitorID, err := tc.GetMonitorByName(ctx, monitorName)
	if err != nil {
		return state, nil, []string{fmt.Sprintf("failed to look up monitor %q: %v", monitorName, err)}
	}
	if monitorID != "" {
		if err := tc.UpdateMonitor(ctx, monitorID, monitorName, monitorDesc, monitorCfg); err != nil {
			return state, nil, []string{fmt.Sprintf("failed to update monitor %q: %v", monitorName, err)}
		}
	} else {
		monitorID, err = tc.CreateMonitor(ctx, monitorName, monitorDesc, monitorCfg)
		if err != nil {
			return state, nil, []string{fmt.Sprintf("failed to create monitor %q: %v", monitorName, err)}
		}
		changes = append(changes, fmt.Sprintf("created monitor %s", monitorName))
		l.V(1).Info("Created monitor", "monitor", monitorName, "monitorID", monitorID)
	}
	state.monitorID = monitorID

	// 2. Compute desired pools.
	desiredPools := computeDesiredPools(gw, mode, params, entries, validRoutes)

	// 3. Ensure pools.
	poolIDMap := make(map[string]string, len(desiredPools)) // pool name -> pool ID
	for _, dp := range desiredPools {
		dp.MonitorID = monitorID
		existingID, _, err := tc.GetPoolByName(ctx, dp.Name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("failed to look up pool %q: %v", dp.Name, err))
			continue
		}
		if existingID != "" {
			if err := tc.UpdatePool(ctx, existingID, dp); err != nil {
				errs = append(errs, fmt.Sprintf("failed to update pool %q: %v", dp.Name, err))
				continue
			}
			poolIDMap[dp.Name] = existingID
		} else {
			poolID, err := tc.CreatePool(ctx, dp)
			if err != nil {
				errs = append(errs, fmt.Sprintf("failed to create pool %q: %v", dp.Name, err))
				continue
			}
			poolIDMap[dp.Name] = poolID
			changes = append(changes, fmt.Sprintf("created pool %s", dp.Name))
			l.V(1).Info("Created pool", "pool", dp.Name, "poolID", poolID)
		}
	}

	// Record pool state.
	for _, dp := range desiredPools {
		if id, ok := poolIDMap[dp.Name]; ok {
			state.pools = append(state.pools, apiv1.PoolStatus{Name: dp.Name, ID: id})
		}
	}

	if len(errs) > 0 {
		return state, changes, errs
	}

	// 4. Compute desired hostnames and ensure load balancers.
	steeringPolicy := apiv1.CloudflareSteeringPolicy(params.Spec.LoadBalancer.SteeringPolicy)
	sessionAffinity := apiv1.CloudflareSessionAffinity(params.Spec.LoadBalancer.SessionAffinity)

	desiredHostnames := computeDesiredHostnames(validRoutes, zoneName)
	state.hostnames = desiredHostnames

	lbDesc := apiv1.ResourceDescription(gw)
	for _, hostname := range desiredHostnames {
		poolIDs, poolWeights := computeLBPoolsForHostname(hostname, mode, gw, params, validRoutes, poolIDMap)
		if err := tc.EnsureLoadBalancer(ctx, zoneID, hostname, poolIDs, steeringPolicy, sessionAffinity, lbDesc, poolWeights); err != nil {
			errs = append(errs, fmt.Sprintf("failed to ensure load balancer for %q: %v", hostname, err))
			continue
		}
		l.V(1).Info("Ensured load balancer", "hostname", hostname)
	}

	if len(errs) > 0 {
		return state, changes, errs
	}

	// 5. Cleanup stale load balancers (in previous CGS but not in desired).
	desiredHostnameSet := make(map[string]struct{}, len(desiredHostnames))
	for _, h := range desiredHostnames {
		desiredHostnameSet[h] = struct{}{}
	}
	for _, h := range previousHostnames {
		if _, ok := desiredHostnameSet[h]; ok {
			continue
		}
		if err := tc.DeleteLoadBalancer(ctx, zoneID, h); err != nil {
			errs = append(errs, fmt.Sprintf("failed to delete stale load balancer for %q: %v", h, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted stale load balancer for %s", h))
		l.V(1).Info("Deleted stale load balancer", "hostname", h)
	}

	// 6. Cleanup stale pools (in previous CGS but not in desired).
	desiredPoolNames := make(map[string]struct{}, len(desiredPools))
	for _, dp := range desiredPools {
		desiredPoolNames[dp.Name] = struct{}{}
	}
	for _, p := range previousPools {
		if _, ok := desiredPoolNames[p.Name]; ok {
			continue
		}
		if p.ID == "" {
			continue
		}
		if err := tc.DeletePool(ctx, p.ID); err != nil {
			errs = append(errs, fmt.Sprintf("failed to delete stale pool %q: %v", p.Name, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted stale pool %s", p.Name))
		l.V(1).Info("Deleted stale pool", "pool", p.Name, "poolID", p.ID)
	}

	return state, changes, errs
}

// cleanupAllLBResources deletes all LB resources for a Gateway: load balancers,
// pools, and the monitor. Used when switching from LB mode to simple mode or
// during finalization. zoneName and zoneID identify the DNS zone; if either is
// empty, no cleanup is performed. ownedHostnames is the list of LB hostnames
// owned by this gateway (from CGS.Status.LoadBalancer.Hostnames); only these
// hostnames are deleted, preventing interference with other gateways or
// external services sharing the same zone. ownedPools is the list of LB pools
// owned by this gateway (from CGS.Status.LoadBalancer.Pools).
func (r *GatewayReconciler) cleanupAllLBResources(
	ctx context.Context,
	tc cloudflare.Client,
	gw *gatewayv1.Gateway,
	zoneName, zoneID string,
	ownedHostnames []string,
	ownedPools []apiv1.PoolStatus,
) ([]string, []string) {
	l := log.FromContext(ctx)
	var changes []string
	var errs []string

	if zoneName == "" || zoneID == "" {
		return nil, nil
	}

	// Delete load balancers first (they reference pools).
	// Only delete hostnames owned by this gateway (from CGS), not all zone LBs.
	for _, h := range ownedHostnames {
		if !hostnameInZone(h, zoneName) {
			continue
		}
		if err := tc.DeleteLoadBalancer(ctx, zoneID, h); err != nil {
			errs = append(errs, fmt.Sprintf("failed to delete load balancer for %q: %v", h, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted load balancer for %s", h))
		l.V(1).Info("Deleted load balancer", "hostname", h)
	}

	// Delete pools (they reference the monitor).
	for _, p := range ownedPools {
		if p.ID == "" {
			continue
		}
		if err := tc.DeletePool(ctx, p.ID); err != nil {
			errs = append(errs, fmt.Sprintf("failed to delete pool %q: %v", p.Name, err))
			continue
		}
		changes = append(changes, fmt.Sprintf("deleted pool %s", p.Name))
		l.V(1).Info("Deleted pool", "pool", p.Name, "poolID", p.ID)
	}

	// Delete monitor.
	monitorName := apiv1.MonitorName(gw)
	monitorID, err := tc.GetMonitorByName(ctx, monitorName)
	if err != nil {
		errs = append(errs, fmt.Sprintf("failed to look up monitor %q for cleanup: %v", monitorName, err))
	} else if monitorID != "" {
		if err := tc.DeleteMonitor(ctx, monitorID); err != nil {
			errs = append(errs, fmt.Sprintf("failed to delete monitor %q: %v", monitorName, err))
		} else {
			changes = append(changes, fmt.Sprintf("deleted monitor %s", monitorName))
			l.V(1).Info("Deleted monitor", "monitor", monitorName, "monitorID", monitorID)
		}
	}

	return changes, errs
}

// computeDesiredPools builds the desired pool configurations based on the LB mode.
func computeDesiredPools(
	gw *gatewayv1.Gateway,
	mode lbMode,
	params *apiv1.CloudflareGatewayParameters,
	entries []tunnelEntry,
	validRoutes []*gatewayv1.HTTPRoute,
) []cloudflare.PoolConfig {
	switch mode {
	case lbModePerAZ:
		// HighAvailability: 1 pool per AZ, each with 1 origin (the AZ tunnel).
		pools := make([]cloudflare.PoolConfig, 0, len(entries))
		for _, e := range entries {
			pools = append(pools, cloudflare.PoolConfig{
				Name:        apiv1.PoolNameForAZ(gw, e.azName),
				Description: apiv1.ResourceDescription(gw, "az:"+e.azName),
				Weight:      1,
				Enabled:     true,
				Origins: []cloudflare.PoolOrigin{{
					Name:    e.azName,
					Address: cloudflare.TunnelTarget(e.tunnelID),
					Enabled: true,
					Weight:  1,
				}},
			})
		}
		return pools

	case lbModePerBackendRef:
		// TrafficSplitting: 1 pool per unique Service.
		services := collectUniqueServices(validRoutes)
		hasAZs := params.Spec.Tunnels != nil && len(params.Spec.Tunnels.AvailabilityZones) > 0

		// Build a lookup from "svcNs/svcName[/azName]" -> tunnelID.
		tunnelLookup := make(map[string]string, len(entries))
		for _, e := range entries {
			key := e.serviceNamespace + "/" + e.serviceName
			if e.azName != "" {
				key += "/" + e.azName
			}
			tunnelLookup[key] = e.tunnelID
		}

		pools := make([]cloudflare.PoolConfig, 0, len(services))
		for _, svc := range services {
			svcKey := svc.Namespace + "/" + svc.Name
			pool := cloudflare.PoolConfig{
				Name:        apiv1.PoolNameForService(gw, svc.Namespace, svc.Name),
				Description: apiv1.ResourceDescription(gw, "svc:"+svcKey),
				Weight:      1,
				Enabled:     true,
			}
			if hasAZs {
				for _, az := range params.Spec.Tunnels.AvailabilityZones {
					key := svcKey + "/" + az.Name
					tunnelID := tunnelLookup[key]
					if tunnelID == "" {
						continue
					}
					pool.Origins = append(pool.Origins, cloudflare.PoolOrigin{
						Name:    az.Name,
						Address: cloudflare.TunnelTarget(tunnelID),
						Enabled: true,
						Weight:  1,
					})
				}
			} else {
				tunnelID := tunnelLookup[svcKey]
				if tunnelID != "" {
					pool.Origins = append(pool.Origins, cloudflare.PoolOrigin{
						Name:    svc.Name,
						Address: cloudflare.TunnelTarget(tunnelID),
						Enabled: true,
						Weight:  1,
					})
				}
			}
			pools = append(pools, pool)
		}
		return pools

	default:
		return nil
	}
}

// computeDesiredHostnames extracts unique hostnames from valid routes that
// fall within the DNS zone, sorted deterministically.
func computeDesiredHostnames(routes []*gatewayv1.HTTPRoute, zoneName string) []string {
	seen := make(map[string]struct{})
	var hostnames []string
	for _, route := range routes {
		for _, h := range route.Spec.Hostnames {
			hostname := string(h)
			if _, ok := seen[hostname]; ok {
				continue
			}
			if hostnameInZone(hostname, zoneName) {
				seen[hostname] = struct{}{}
				hostnames = append(hostnames, hostname)
			}
		}
	}
	return hostnames
}

// computeLBPoolsForHostname computes the pool IDs and pool weights for a
// specific hostname's load balancer.
func computeLBPoolsForHostname(
	hostname string,
	mode lbMode,
	gw *gatewayv1.Gateway,
	params *apiv1.CloudflareGatewayParameters,
	validRoutes []*gatewayv1.HTTPRoute,
	poolIDMap map[string]string,
) (poolIDs []string, poolWeights map[string]float64) {
	switch mode {
	case lbModePerAZ:
		// HighAvailability: all AZ pools serve every hostname.
		for _, az := range params.Spec.Tunnels.AvailabilityZones {
			poolName := apiv1.PoolNameForAZ(gw, az.Name)
			if id, ok := poolIDMap[poolName]; ok {
				poolIDs = append(poolIDs, id)
			}
		}
		return poolIDs, nil

	case lbModePerBackendRef:
		// TrafficSplitting: only pools for services referenced on this hostname,
		// with weights from backendRef weights. Key is "namespace/name".
		type svcKey struct{ Namespace, Name string }
		weightSums := make(map[svcKey]int32)
		for _, route := range validRoutes {
			routeHasHostname := false
			for _, h := range route.Spec.Hostnames {
				if string(h) == hostname {
					routeHasHostname = true
					break
				}
			}
			if !routeHasHostname {
				continue
			}
			for _, rule := range route.Spec.Rules {
				for _, ref := range rule.BackendRefs {
					ns := route.Namespace
					if ref.Namespace != nil {
						ns = string(*ref.Namespace)
					}
					key := svcKey{Namespace: ns, Name: string(ref.Name)}
					weight := int32(1)
					if ref.Weight != nil {
						weight = *ref.Weight
					}
					weightSums[key] += weight
				}
			}
		}

		// Normalize weights to [0, 1].
		var totalWeight int32
		for _, w := range weightSums {
			totalWeight += w
		}

		poolWeights = make(map[string]float64, len(weightSums))
		for svc, w := range weightSums {
			poolName := apiv1.PoolNameForService(gw, svc.Namespace, svc.Name)
			if id, ok := poolIDMap[poolName]; ok {
				poolIDs = append(poolIDs, id)
				if totalWeight > 0 {
					poolWeights[id] = float64(w) / float64(totalWeight)
				}
			}
		}
		return poolIDs, poolWeights

	default:
		return nil, nil
	}
}
