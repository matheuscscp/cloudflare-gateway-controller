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
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
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
	testMock   *mockCloudflareClient
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

	testMock = &mockCloudflareClient{
		tunnelID:    "test-tunnel-id",
		tunnelToken: "test-tunnel-token",
	}
	if err := (&controller.GatewayReconciler{
		Client:        mgr.GetClient(),
		EventRecorder: eventRecorder,
		NewCloudflareClient: func(_ cloudflare.ClientConfig) (cloudflare.Client, error) {
			return testMock, nil
		},
		CloudflaredImage: controller.DefaultCloudflaredImage,
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

type mockCloudflareClient struct {
	tunnelID     string
	tunnelToken  string
	deleteCalled bool
	deletedID    string

	// HTTPRoute-related tracking
	lastTunnelConfigID      string
	lastTunnelConfigIngress []cloudflare.IngressRule
	ensureDNSCalls          []mockDNSCall
	deleteDNSCalls          []mockDNSCall
	zones                   map[string]string // hostname -> zoneID
	zoneIDs                 []string          // zone IDs returned by ListZoneIDs
	listDNSCNAMEsByTarget   []string          // hostnames returned by ListDNSCNAMEsByTarget
}

type mockDNSCall struct {
	ZoneID   string
	Hostname string
	Target   string
}

func (m *mockCloudflareClient) CreateTunnel(_ context.Context, _ string) (string, error) {
	return m.tunnelID, nil
}

func (m *mockCloudflareClient) GetTunnelIDByName(_ context.Context, _ string) (string, error) {
	return m.tunnelID, nil
}

func (m *mockCloudflareClient) DeleteTunnel(_ context.Context, tunnelID string) error {
	m.deleteCalled = true
	m.deletedID = tunnelID
	return nil
}

func (m *mockCloudflareClient) GetTunnelToken(_ context.Context, _ string) (string, error) {
	return m.tunnelToken, nil
}

func (m *mockCloudflareClient) UpdateTunnelConfiguration(_ context.Context, tunnelID string, ingress []cloudflare.IngressRule) error {
	m.lastTunnelConfigID = tunnelID
	m.lastTunnelConfigIngress = ingress
	return nil
}

func (m *mockCloudflareClient) ListZoneIDs(_ context.Context) ([]string, error) {
	return m.zoneIDs, nil
}

func (m *mockCloudflareClient) FindZoneIDByHostname(_ context.Context, hostname string) (string, error) {
	if m.zones != nil {
		if zoneID, ok := m.zones[hostname]; ok {
			return zoneID, nil
		}
	}
	return "test-zone-id", nil
}

func (m *mockCloudflareClient) EnsureDNSCNAME(_ context.Context, zoneID, hostname, target string) error {
	m.ensureDNSCalls = append(m.ensureDNSCalls, mockDNSCall{ZoneID: zoneID, Hostname: hostname, Target: target})
	return nil
}

func (m *mockCloudflareClient) DeleteDNSCNAME(_ context.Context, zoneID, hostname string) error {
	m.deleteDNSCalls = append(m.deleteDNSCalls, mockDNSCall{ZoneID: zoneID, Hostname: hostname})
	return nil
}

func (m *mockCloudflareClient) ListDNSCNAMEsByTarget(_ context.Context, _, _ string) ([]string, error) {
	return m.listDNSCNAMEsByTarget, nil
}

func createTestNamespace(g Gomega) *corev1.Namespace {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	g.Expect(testClient.Create(testCtx, ns)).To(Succeed())
	return ns
}

func createTestSecret(g Gomega, namespace string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloudflare-creds",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"CLOUDFLARE_API_TOKEN":  []byte("test-api-token"),
			"CLOUDFLARE_ACCOUNT_ID": []byte("test-account-id"),
		},
	}
	g.Expect(testClient.Create(testCtx, secret)).To(Succeed())
	return secret
}

func createTestGatewayClass(g Gomega, name string, secretNamespace string) *gatewayv1.GatewayClass {
	ns := gatewayv1.Namespace(secretNamespace)
	gc := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: apiv1.ControllerName,
			ParametersRef: &gatewayv1.ParametersReference{
				Group:     "",
				Kind:      "Secret",
				Name:      "cloudflare-creds",
				Namespace: &ns,
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gc)).To(Succeed())
	return gc
}

func createTestGateway(g Gomega, name, namespace, gcName string) *gatewayv1.Gateway {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gcName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Protocol: gatewayv1.HTTPProtocolType,
					Port:     80,
				},
			},
		},
	}
	g.Expect(testClient.Create(testCtx, gw)).To(Succeed())
	return gw
}

func waitForGatewayClassReady(g Gomega, gc *gatewayv1.GatewayClass) {
	key := client.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func waitForGatewayProgrammed(g Gomega, gw *gatewayv1.Gateway) {
	key := client.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
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
