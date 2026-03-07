// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

const (
	cliFieldManager = "cfgwctl"
	pollInterval    = 2 * time.Second
)

// kubeClients holds the various Kubernetes clients used by CLI commands.
type kubeClients struct {
	Client    client.Client
	Namespace string
	Config    *rest.Config
}

// buildKubeClient creates a controller-runtime client from kubeconfig.
// If flagNamespace is empty, it uses the namespace from the kubeconfig context.
func buildKubeClient(flagNamespace string) (client.Client, string, error) {
	kc, err := buildKubeClients(flagNamespace)
	if err != nil {
		return nil, "", err
	}
	return kc.Client, kc.Namespace, nil
}

func buildKubeClients(flagNamespace string) (*kubeClients, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	if flagNamespace != "" {
		configOverrides.Context.Namespace = flagNamespace
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	cfg, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	ns, _, err := kubeConfig.Namespace()
	if err != nil {
		return nil, fmt.Errorf("determining namespace: %w", err)
	}

	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(gatewayv1.Install(s))
	utilruntime.Must(apiv1.Install(s))

	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	return &kubeClients{Client: c, Namespace: ns, Config: cfg}, nil
}

// snapshotLastHandledReconcileAt fetches the CGS and returns the current
// LastHandledReconcileAt value. This is used as the "before" snapshot for
// wait logic. If the CGS does not exist yet, returns "" (any future value
// will differ).
func snapshotLastHandledReconcileAt(ctx context.Context, c client.Client, name, namespace string) (string, error) {
	var cgs apiv1.CloudflareGatewayStatus
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &cgs); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("getting CloudflareGatewayStatus: %w", err)
	}
	return cgs.Status.LastHandledReconcileAt, nil
}

// waitForReconciliation polls the CGS until LastHandledReconcileAt differs
// from oldValue (meaning the controller has handled a reconciliation since
// we snapshotted), then checks the Ready condition.
//
// This follows the Flux CLI pattern:
//  1. Any change from the snapshot means some reconciliation completed.
//  2. After detecting the change, check Ready condition for health.
//  3. If Ready=False with a terminal reason, return the error immediately.
//  4. If Ready is not yet True (e.g. Progressing), keep polling.
func waitForReconciliation(ctx context.Context, c client.Client, name, namespace, oldValue string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for reconciliation")
		case <-ticker.C:
			var cgs apiv1.CloudflareGatewayStatus
			if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &cgs); err != nil {
				continue // CGS may not exist yet, keep polling
			}

			// Step 1: Wait for the status field to change from the snapshot.
			if cgs.Status.LastHandledReconcileAt == oldValue {
				continue
			}

			// Step 2: Status field changed — check the Ready condition.
			readyCond := meta.FindStatusCondition(cgs.Status.Conditions, apiv1.ConditionReady)
			if readyCond == nil {
				continue // Conditions not yet set, keep polling
			}

			switch readyCond.Status {
			case metav1.ConditionTrue:
				return nil // Reconciliation succeeded
			case metav1.ConditionFalse:
				// Terminal failure — stop polling immediately.
				return fmt.Errorf("reconciliation failed: %s", readyCond.Message)
			default:
				continue // Still progressing, keep polling
			}
		}
	}
}
