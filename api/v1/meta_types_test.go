// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func TestTunnelName(t *testing.T) {
	g := NewWithT(t)
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("abc-123")},
	}
	g.Expect(apiv1.TunnelName(gw)).To(Equal("gateway-abc-123"))
}

func TestCloudflaredDeploymentName(t *testing.T) {
	g := NewWithT(t)
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	g.Expect(apiv1.CloudflaredDeploymentName(gw)).To(Equal("cloudflared-my-gw"))
}

func TestTunnelTokenSecretName(t *testing.T) {
	g := NewWithT(t)
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	g.Expect(apiv1.TunnelTokenSecretName(gw)).To(Equal("cloudflared-token-my-gw"))
}

func TestFinalizerGatewayClass(t *testing.T) {
	g := NewWithT(t)
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gw",
			Namespace: "my-ns",
		},
	}
	g.Expect(apiv1.FinalizerGatewayClass(gw)).To(Equal("gateway-exists-finalizer.gateway.networking.k8s.io/my-gw.my-ns"))
}

func TestReconcileInterval(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        time.Duration
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
			name: "invalid duration returns default",
			annotations: map[string]string{
				apiv1.AnnotationReconcileEvery: "not-a-duration",
			},
			want: apiv1.DefaultReconcileInterval,
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
			g.Expect(apiv1.ReconcileInterval(tt.annotations)).To(Equal(tt.want))
		})
	}
}
