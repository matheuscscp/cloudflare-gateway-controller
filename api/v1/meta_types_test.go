// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func init() {
	apiv1.SetClusterName("test-cluster")
}

func testGateway() *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gw",
			Namespace: "my-ns",
		},
	}
}

func TestResourceName(t *testing.T) {
	g := NewWithT(t)
	name := apiv1.ResourceName("a", "b", "c")
	g.Expect(name).To(HavePrefix("gw-"))
	// Full SHA256 = 64 hex chars, "gw-" prefix = 67 chars total.
	g.Expect(name).To(HaveLen(67))
	// Same inputs produce same output.
	g.Expect(apiv1.ResourceName("a", "b", "c")).To(Equal(name))
	// Different inputs produce different output.
	g.Expect(apiv1.ResourceName("a", "b", "d")).NotTo(Equal(name))
}

func TestResourceDescription(t *testing.T) {
	g := NewWithT(t)
	gw := testGateway()
	desc := apiv1.ResourceDescription(gw)
	g.Expect(desc).To(Equal("cfgw cluster:test-cluster ns:my-ns gw:my-gw"))
	descWithExtra := apiv1.ResourceDescription(gw, "az:us-east-1a", "svc:default/web")
	g.Expect(descWithExtra).To(Equal("cfgw cluster:test-cluster ns:my-ns gw:my-gw az:us-east-1a svc:default/web"))
}

func TestTunnelName(t *testing.T) {
	g := NewWithT(t)
	gw := testGateway()
	name := apiv1.TunnelName(gw)
	g.Expect(name).To(HavePrefix("gw-"))
	g.Expect(name).To(HaveLen(67))
	// Deterministic: same cluster/ns/gw always produces the same name.
	g.Expect(apiv1.TunnelName(gw)).To(Equal(name))
}

func TestGatewayResourceName(t *testing.T) {
	g := NewWithT(t)
	gw := testGateway()
	g.Expect(apiv1.GatewayResourceName(gw)).To(Equal("gateway-my-gw"))
}

func TestFinalizerGatewayClass(t *testing.T) {
	g := NewWithT(t)
	gw := testGateway()
	g.Expect(apiv1.FinalizerGatewayClass(gw)).To(Equal("gateway-exists-finalizer.gateway.networking.k8s.io/my-gw.my-ns"))
}

func TestReconcileInterval(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        time.Duration
		wantErr     bool
	}{
		{
			name:        "nil annotations returns default",
			annotations: nil,
			want:        apiv1.DefaultReconcileInterval,
		},
		{
			name:        "empty annotations returns default",
			annotations: map[string]string{},
			want:        apiv1.DefaultReconcileInterval,
		},
		{
			name: "disabled returns zero",
			annotations: map[string]string{
				apiv1.AnnotationReconcile: apiv1.ValueDisabled,
			},
			want: 0,
		},
		{
			name: "custom interval",
			annotations: map[string]string{
				apiv1.AnnotationReconcileEvery: "5m",
			},
			want: 5 * time.Minute,
		},
		{
			name: "invalid duration returns error",
			annotations: map[string]string{
				apiv1.AnnotationReconcileEvery: "not-a-duration",
			},
			wantErr: true,
		},
		{
			name: "disabled takes precedence over custom interval",
			annotations: map[string]string{
				apiv1.AnnotationReconcile:      apiv1.ValueDisabled,
				apiv1.AnnotationReconcileEvery: "5m",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			interval, err := apiv1.ReconcileInterval(tt.annotations)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(interval).To(Equal(tt.want))
			}
		})
	}
}
