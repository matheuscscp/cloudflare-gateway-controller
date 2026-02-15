// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"os"
	"testing"

	semver "github.com/Masterminds/semver/v3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	cfclient "github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// testGatewayAPIVersion must match the major.minor version of the
// sigs.k8s.io/gateway-api dependency in go.mod. This is updated
// automatically by the upgrade-gateway-api workflow.
var testGatewayAPIVersion = semver.MustParse("1.4.0")

var (
	testEnv    *envtest.Environment
	testClient client.Client
	testCtx    context.Context
	testCancel context.CancelFunc
	testMock   *mockTunnelClient
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(gatewayv1.AddToScheme(s))
	return s
}

func TestMain(m *testing.M) {
	testCtx, testCancel = context.WithCancel(context.Background())

	scheme := newTestScheme()

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{os.Getenv("GATEWAY_API_CRDS")},
		Scheme:            scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start test environment: %v", err))
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create manager: %v", err))
	}

	if err := (&GatewayClassReconciler{
		Client:            mgr.GetClient(),
		GatewayAPIVersion: testGatewayAPIVersion,
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup GatewayClass controller: %v", err))
	}

	testMock = &mockTunnelClient{
		createTunnelID: "test-tunnel-id",
		tunnelToken:    "test-tunnel-token",
	}
	if err := (&GatewayReconciler{
		Client: mgr.GetClient(),
		NewTunnelClient: func(_ cfclient.ClientConfig) (cfclient.TunnelClient, error) {
			return testMock, nil
		},
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup Gateway controller: %v", err))
	}

	testClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Sprintf("failed to create test client: %v", err))
	}

	go func() {
		fmt.Println("Starting the test environment")
		if err := mgr.Start(testCtx); err != nil {
			panic(fmt.Sprintf("failed to start manager: %v", err))
		}
	}()

	code := m.Run()

	fmt.Println("Stopping the test environment")
	testCancel()
	if err := testEnv.Stop(); err != nil {
		panic(fmt.Sprintf("failed to stop test environment: %v", err))
	}

	os.Exit(code)
}
