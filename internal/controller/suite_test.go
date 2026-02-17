// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	semver "github.com/Masterminds/semver/v3"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	eventsv1 "k8s.io/api/events/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	cfclient "github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/controller"
)

// testGatewayAPIVersion must match the major.minor version of the
// sigs.k8s.io/gateway-api dependency in go.mod. This is updated
// automatically by the upgrade-gateway-api workflow.
var testGatewayAPIVersion = semver.MustParse("1.4.1")

var (
	testEnv    *envtest.Environment
	testClient client.Client
	testCtx    context.Context
	testCancel context.CancelFunc
	testMock   *mockTunnelClient
)

// gatewayAPICRDPath returns the path to the Gateway API CRD directory.
// It computes it from GOMODCACHE and testGatewayAPIVersion.
func gatewayAPICRDPath() string {
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		panic(fmt.Sprintf("failed to run 'go env GOMODCACHE': %v", err))
	}
	gomodcache := strings.TrimSpace(string(out))

	return filepath.Join(gomodcache, "sigs.k8s.io", "gateway-api@v"+testGatewayAPIVersion.Original(), "config", "crd", "standard")
}

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(apiextensionsv1.AddToScheme(s))
	utilruntime.Must(gatewayv1.Install(s))
	utilruntime.Must(gatewayv1beta1.Install(s))
	return s
}

func TestMain(m *testing.M) {
	testCtx, testCancel = context.WithCancel(context.Background())

	scheme := newTestScheme()

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{gatewayAPICRDPath()},
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

	eventRecorder := mgr.GetEventRecorder(apiv1.ShortControllerName)

	if err := (&controller.GatewayClassReconciler{
		Client:            mgr.GetClient(),
		EventRecorder:     eventRecorder,
		GatewayAPIVersion: *testGatewayAPIVersion,
	}).SetupWithManager(testCtx, mgr); err != nil {
		panic(fmt.Sprintf("failed to setup GatewayClass controller: %v", err))
	}

	testMock = &mockTunnelClient{
		tunnelID:    "test-tunnel-id",
		tunnelToken: "test-tunnel-token",
	}
	if err := (&controller.GatewayReconciler{
		Client:        mgr.GetClient(),
		EventRecorder: eventRecorder,
		NewTunnelClient: func(_ cfclient.ClientConfig) (cfclient.TunnelClient, error) {
			return testMock, nil
		},
		CloudflaredImage: controller.DefaultCloudflaredImage,
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup Gateway controller: %v", err))
	}

	if err := (&controller.HTTPRouteReconciler{
		Client:        mgr.GetClient(),
		EventRecorder: eventRecorder,
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup HTTPRoute controller: %v", err))
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

	// Simulate Deployment readiness since envtest has no Deployment controller.
	go func() {
		for {
			select {
			case <-testCtx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			var list appsv1.DeploymentList
			if err := testClient.List(testCtx, &list); err != nil {
				continue
			}
			for i := range list.Items {
				deploy := &list.Items[i]
				desired := int32(1)
				if deploy.Spec.Replicas != nil {
					desired = *deploy.Spec.Replicas
				}
				if deploy.Status.ReadyReplicas == desired {
					continue
				}
				deploy.Status.Replicas = desired
				deploy.Status.ReadyReplicas = desired
				deploy.Status.AvailableReplicas = desired
				deploy.Status.UpdatedReplicas = desired
				deploy.Status.ObservedGeneration = deploy.Generation
				deploy.Status.Conditions = []appsv1.DeploymentCondition{
					{
						Type:   appsv1.DeploymentAvailable,
						Status: "True",
						Reason: "MinimumReplicasAvailable",
					},
					{
						Type:   appsv1.DeploymentProgressing,
						Status: "True",
						Reason: "NewReplicaSetAvailable",
					},
				}
				_ = testClient.Status().Update(testCtx, deploy)
			}
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

// findEvent returns the first events.k8s.io/v1 Event for the given object name
// matching the specified type and reason.
func findEvent(g Gomega, objectName, eventType, reason string) *eventsv1.Event {
	var events eventsv1.EventList
	g.Expect(testClient.List(testCtx, &events, client.InNamespace("default"))).To(Succeed())
	for i, e := range events.Items {
		if e.Regarding.Name == objectName && e.Type == eventType && e.Reason == reason {
			return &events.Items[i]
		}
	}
	return nil
}
