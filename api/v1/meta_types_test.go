// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package v1_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func TestTunnelName(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("abc-123")},
	}
	if got := apiv1.TunnelName(gw); got != "gateway-abc-123" {
		t.Errorf("TunnelName() = %q, want %q", got, "gateway-abc-123")
	}
}

func TestCloudflaredDeploymentName(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	if got := apiv1.CloudflaredDeploymentName(gw); got != "cloudflared-my-gw" {
		t.Errorf("CloudflaredDeploymentName() = %q, want %q", got, "cloudflared-my-gw")
	}
}

func TestTunnelTokenSecretName(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw"},
	}
	if got := apiv1.TunnelTokenSecretName(gw); got != "cloudflared-token-my-gw" {
		t.Errorf("TunnelTokenSecretName() = %q, want %q", got, "cloudflared-token-my-gw")
	}
}

func TestFinalizerGatewayClass(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gw",
			Namespace: "my-ns",
		},
	}
	want := "gateway-exists-finalizer.gateway.networking.k8s.io/my-gw.my-ns"
	if got := apiv1.FinalizerGatewayClass(gw); got != want {
		t.Errorf("FinalizerGatewayClass() = %q, want %q", got, want)
	}
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
			if got := apiv1.ReconcileInterval(tt.annotations); got != tt.want {
				t.Errorf("ReconcileInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}
