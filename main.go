// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/Masterminds/semver/v3"
	"github.com/fluxcd/pkg/runtime/logger"
	"github.com/fluxcd/pkg/ssa"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
}

func main() {
	ctx := ctrl.SetupSignalHandler()

	cloudflaredImage := pflag.String("cloudflared-image", controller.DefaultCloudflaredImage, "cloudflared container image")
	leaderElect := pflag.Bool("leader-elect", true, "enable leader election")

	logOptions := logger.Options{}
	logOptions.BindFlags(pflag.CommandLine)
	pflag.Parse()

	logger.SetLogger(logger.NewLogger(logOptions))

	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         *leaderElect,
		LeaderElectionID:       apiv1.ShortControllerName,
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
		os.Exit(1)
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
		GatewayAPIVersion: gatewayAPIVersion(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GatewayClass")
		os.Exit(1)
	}

	if err := (&controller.GatewayReconciler{
		Client:              client,
		EventRecorder:       eventRecorder,
		ResourceManager:     resourceManager,
		NewCloudflareClient: cloudflare.NewClient,
		CloudflaredImage:    *cloudflaredImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Gateway")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "unable to start controller")
		os.Exit(1)
	}
}

// gatewayAPIVersion returns the parsed semver version of the
// sigs.k8s.io/gateway-api module dependency from the build info.
func gatewayAPIVersion() semver.Version {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		panic("failed to read build info")
	}
	for _, dep := range info.Deps {
		if dep.Path == "sigs.k8s.io/gateway-api" {
			v, err := semver.NewVersion(dep.Version)
			if err != nil {
				panic(fmt.Sprintf("failed to parse gateway-api version '%s': %v", dep.Version, err))
			}
			return *v
		}
	}
	panic("gateway-api dependency not found in build info")
}
