// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1_test

import (
	"testing"

	. "github.com/onsi/gomega"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func TestCloudflareSteeringPolicy(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{apiv1.SteeringPolicyOff, apiv1.CloudflareSteeringPolicyOff},
		{apiv1.SteeringPolicyGeographic, apiv1.CloudflareSteeringPolicyGeo},
		{apiv1.SteeringPolicyRandom, apiv1.CloudflareSteeringPolicyRandom},
		{apiv1.SteeringPolicyDynamicLatency, apiv1.CloudflareSteeringPolicyDynamicLatency},
		{apiv1.SteeringPolicyProximity, apiv1.CloudflareSteeringPolicyProximity},
		{apiv1.SteeringPolicyLeastOutstandingRequests, apiv1.CloudflareSteeringPolicyLeastOutstandingRequests},
		{apiv1.SteeringPolicyLeastConnections, apiv1.CloudflareSteeringPolicyLeastConnections},
		{"", apiv1.CloudflareSteeringPolicyOff},
		{"Unknown", apiv1.CloudflareSteeringPolicyOff},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(apiv1.CloudflareSteeringPolicy(tt.input)).To(Equal(tt.want))
		})
	}
}

func TestCloudflareSessionAffinity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{apiv1.SessionAffinityNone, apiv1.CloudflareSessionAffinityNone},
		{apiv1.SessionAffinityCookie, apiv1.CloudflareSessionAffinityCookie},
		{apiv1.SessionAffinityIPCookie, apiv1.CloudflareSessionAffinityIPCookie},
		{apiv1.SessionAffinityHeader, apiv1.CloudflareSessionAffinityHeader},
		{"", apiv1.CloudflareSessionAffinityNone},
		{"Unknown", apiv1.CloudflareSessionAffinityNone},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(apiv1.CloudflareSessionAffinity(tt.input)).To(Equal(tt.want))
		})
	}
}

func TestCloudflareMonitorType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{apiv1.MonitorTypeHTTP, apiv1.CloudflareMonitorTypeHTTP},
		{apiv1.MonitorTypeHTTPS, apiv1.CloudflareMonitorTypeHTTPS},
		{apiv1.MonitorTypeTCP, apiv1.CloudflareMonitorTypeTCP},
		{"", apiv1.CloudflareMonitorTypeHTTPS},
		{"Unknown", apiv1.CloudflareMonitorTypeHTTPS},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(apiv1.CloudflareMonitorType(tt.input)).To(Equal(tt.want))
		})
	}
}

func TestResolveMonitorConfig(t *testing.T) {
	t.Run("nil LoadBalancerConfig", func(t *testing.T) {
		g := NewWithT(t)
		cfg := apiv1.ResolveMonitorConfig(nil)
		assertMonitorDefaults(g, cfg)
	})

	t.Run("nil Monitor", func(t *testing.T) {
		g := NewWithT(t)
		cfg := apiv1.ResolveMonitorConfig(&apiv1.LoadBalancerConfig{
			Topology: apiv1.LoadBalancerTopologyHighAvailability,
		})
		assertMonitorDefaults(g, cfg)
	})

	t.Run("partial overrides", func(t *testing.T) {
		g := NewWithT(t)
		cfg := apiv1.ResolveMonitorConfig(&apiv1.LoadBalancerConfig{
			Monitor: &apiv1.LoadBalancerMonitorConfig{
				Type: apiv1.MonitorTypeHTTP,
				Path: "/healthz",
			},
		})
		g.Expect(cfg.Type).To(Equal(apiv1.CloudflareMonitorTypeHTTP))
		g.Expect(cfg.Path).To(Equal("/healthz"))
		g.Expect(cfg.Interval).To(Equal(apiv1.DefaultMonitorInterval))
		g.Expect(cfg.Timeout).To(Equal(apiv1.DefaultMonitorTimeout))
		g.Expect(cfg.ExpectedCodes).To(Equal(apiv1.DefaultMonitorExpectedCodes))
	})

	t.Run("full overrides", func(t *testing.T) {
		g := NewWithT(t)
		cfg := apiv1.ResolveMonitorConfig(&apiv1.LoadBalancerConfig{
			Monitor: &apiv1.LoadBalancerMonitorConfig{
				Type:     apiv1.MonitorTypeTCP,
				Path:     "/status",
				Interval: 30,
				Timeout:  10,
			},
		})
		g.Expect(cfg.Type).To(Equal(apiv1.CloudflareMonitorTypeTCP))
		g.Expect(cfg.Path).To(Equal("/status"))
		g.Expect(cfg.Interval).To(Equal(int64(30)))
		g.Expect(cfg.Timeout).To(Equal(int64(10)))
	})
}

func assertMonitorDefaults(g Gomega, cfg apiv1.ResolvedMonitorConfig) {
	g.Expect(cfg.Type).To(Equal(apiv1.DefaultMonitorType))
	g.Expect(cfg.Path).To(Equal(apiv1.DefaultMonitorPath))
	g.Expect(cfg.Interval).To(Equal(apiv1.DefaultMonitorInterval))
	g.Expect(cfg.Timeout).To(Equal(apiv1.DefaultMonitorTimeout))
	g.Expect(cfg.ExpectedCodes).To(Equal(apiv1.DefaultMonitorExpectedCodes))
}
