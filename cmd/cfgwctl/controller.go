// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"runtime/debug"

	"github.com/Masterminds/semver/v3"
	"github.com/fluxcd/pkg/runtime/leaderelection"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/ssa"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/controller"
)

var controllerScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(controllerScheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(controllerScheme))
	utilruntime.Must(vpav1.AddToScheme(controllerScheme))
	utilruntime.Must(gatewayv1.Install(controllerScheme))
	utilruntime.Must(gatewayv1beta1.Install(controllerScheme))
	utilruntime.Must(apiv1.Install(controllerScheme))
}

// getGatewayAPIVersion returns the parsed semver version of the
// sigs.k8s.io/gateway-api module dependency from the build info.
func getGatewayAPIVersion() (*semver.Version, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, fmt.Errorf("failed to read build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "sigs.k8s.io/gateway-api" {
			v, err := semver.NewVersion(dep.Version)
			if err != nil {
				return nil, fmt.Errorf("failed to parse gateway-api version '%s': %v", dep.Version, err)
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("gateway-api dependency not found in build info")
}

type controllerOptions struct {
	clusterName           string
	tunnelImage           string
	logOptions            logger.Options
	leaderElectionOptions leaderelection.Options
}

func newControllerCmd() *cobra.Command {
	opts := &controllerOptions{}

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the Kubernetes controller",
		RunE:  opts.run,
	}

	cmd.Flags().StringVar(&opts.clusterName, "cluster-name", "",
		"unique cluster identifier for deterministic Cloudflare resource naming (required)")
	cobra.CheckErr(cmd.MarkFlagRequired("cluster-name"))

	cmd.Flags().StringVar(&opts.tunnelImage, "tunnel-image", "",
		"tunnel container image (required)")
	cobra.CheckErr(cmd.MarkFlagRequired("tunnel-image"))

	opts.logOptions.BindFlags(cmd.Flags())
	opts.leaderElectionOptions.BindFlags(cmd.Flags())

	return cmd
}

func (o *controllerOptions) run(cmd *cobra.Command, _ []string) error {
	ctx := ctrl.SetupSignalHandler()

	apiv1.SetClusterName(o.clusterName)

	// Enable leader election by default.
	if !cmd.Flags().Changed("enable-leader-election") {
		o.leaderElectionOptions.Enable = true
	}

	logger.SetLogger(logger.NewLogger(o.logOptions))

	setupLog := ctrl.Log.WithName("setup")

	gatewayAPIVersion, err := getGatewayAPIVersion()
	if err != nil {
		return fmt.Errorf("error determining gateway-api version: %w", err)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: controllerScheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress:        ":8081",
		LeaderElection:                o.leaderElectionOptions.Enable,
		LeaderElectionID:              apiv1.ShortControllerName,
		LeaderElectionReleaseOnCancel: o.leaderElectionOptions.ReleaseOnCancel,
		LeaseDuration:                 &o.leaderElectionOptions.LeaseDuration,
		RenewDeadline:                 &o.leaderElectionOptions.RenewDeadline,
		RetryPeriod:                   &o.leaderElectionOptions.RetryPeriod,
		Client: ctrlclient.Options{
			Cache: &ctrlclient.CacheOptions{
				DisableFor: []ctrlclient.Object{
					&corev1.Secret{},
					&corev1.ConfigMap{},
					&apiextensionsv1.CustomResourceDefinition{},
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		return err
	}

	controller.SetupIndexes(ctx, mgr)

	client := mgr.GetClient()
	eventRecorder := mgr.GetEventRecorder(apiv1.ShortControllerName)
	resourceManager := ssa.NewResourceManager(client, nil, ssa.Owner{
		Field: apiv1.ShortControllerName,
	})

	if err := (&controller.GatewayClassReconciler{
		Client:            client,
		EventRecorder:     eventRecorder,
		GatewayAPIVersion: *gatewayAPIVersion,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GatewayClass")
		return err
	}

	if err := (&controller.GatewayReconciler{
		Client:          client,
		EventRecorder:   eventRecorder,
		ResourceManager: resourceManager,
		TunnelImage:     o.tunnelImage,
		NewCloudflareClient: func(cfg cloudflare.ClientConfig) (cloudflare.Client, error) {
			c, err := cloudflare.NewClient(cfg)
			if err != nil {
				return nil, err
			}
			return cloudflare.WithRetry(c), nil
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Gateway")
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		return err
	}

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "unable to start controller")
		return err
	}

	return nil
}
