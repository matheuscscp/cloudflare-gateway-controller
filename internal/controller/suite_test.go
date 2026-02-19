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
	"github.com/fluxcd/pkg/ssa"
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
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
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
	testClient ctrlclient.Client
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

	controller.SetupIndexes(testCtx, mgr)

	client := mgr.GetClient()
	eventRecorder := mgr.GetEventRecorder(apiv1.ShortControllerName)

	if err := (&controller.GatewayClassReconciler{
		Client:            client,
		EventRecorder:     eventRecorder,
		GatewayAPIVersion: *testGatewayAPIVersion,
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup GatewayClass controller: %v", err))
	}

	testMock = &mockCloudflareClient{
		tunnelID:    "test-tunnel-id",
		tunnelToken: "test-tunnel-token",
	}
	if err := (&controller.GatewayReconciler{
		Client:        client,
		EventRecorder: eventRecorder,
		ResourceManager: ssa.NewResourceManager(client, nil, ssa.Owner{
			Field: apiv1.ShortControllerName,
		}),
		NewCloudflareClient: func(cfg cloudflare.ClientConfig) (cloudflare.Client, error) {
			testMock.lastClientConfig = cfg
			return testMock, nil
		},
		CloudflaredImage: controller.DefaultCloudflaredImage,
	}).SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup Gateway controller: %v", err))
	}

	testClient, err = ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
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

	// Credentials tracking
	lastClientConfig cloudflare.ClientConfig

	// HTTPRoute-related tracking
	lastTunnelConfigID      string
	lastTunnelConfigIngress []cloudflare.IngressRule
	ensureDNSCalls          []mockDNSCall
	deleteDNSCalls          []mockDNSCall
	zones                   map[string]string // hostname -> zoneID
	zoneIDs                 []string          // zone IDs returned by ListZoneIDs
	listDNSCNAMEsByTarget   []string          // hostnames returned by ListDNSCNAMEsByTarget

	// Error injection fields — set these to make mock methods return errors.
	getTunnelIDByNameErr      error
	createTunnelErr           error
	deleteTunnelErr           error
	getTunnelTokenErr         error
	getTunnelConfigurationErr error
	updateTunnelConfigErr     error
	listZoneIDsErr            error
	findZoneIDErr             error
	ensureDNSErr              error
	deleteDNSErr              error
	listDNSCNAMEsByTargetErr  error
}

type mockDNSCall struct {
	ZoneID   string
	Hostname string
	Target   string
}

func (m *mockCloudflareClient) CreateTunnel(_ context.Context, _ string) (string, error) {
	if m.createTunnelErr != nil {
		return "", m.createTunnelErr
	}
	return m.tunnelID, nil
}

func (m *mockCloudflareClient) GetTunnelIDByName(_ context.Context, _ string) (string, error) {
	if m.getTunnelIDByNameErr != nil {
		return "", m.getTunnelIDByNameErr
	}
	return m.tunnelID, nil
}

func (m *mockCloudflareClient) DeleteTunnel(_ context.Context, tunnelID string) error {
	if m.deleteTunnelErr != nil {
		return m.deleteTunnelErr
	}
	m.deleteCalled = true
	m.deletedID = tunnelID
	return nil
}

func (m *mockCloudflareClient) GetTunnelToken(_ context.Context, _ string) (string, error) {
	if m.getTunnelTokenErr != nil {
		return "", m.getTunnelTokenErr
	}
	return m.tunnelToken, nil
}

func (m *mockCloudflareClient) GetTunnelConfiguration(_ context.Context, _ string) ([]cloudflare.IngressRule, error) {
	if m.getTunnelConfigurationErr != nil {
		return nil, m.getTunnelConfigurationErr
	}
	return m.lastTunnelConfigIngress, nil
}

func (m *mockCloudflareClient) UpdateTunnelConfiguration(_ context.Context, tunnelID string, ingress []cloudflare.IngressRule) error {
	if m.updateTunnelConfigErr != nil {
		return m.updateTunnelConfigErr
	}
	m.lastTunnelConfigID = tunnelID
	m.lastTunnelConfigIngress = ingress
	return nil
}

func (m *mockCloudflareClient) ListZoneIDs(_ context.Context) ([]string, error) {
	if m.listZoneIDsErr != nil {
		return nil, m.listZoneIDsErr
	}
	return m.zoneIDs, nil
}

func (m *mockCloudflareClient) FindZoneIDByHostname(_ context.Context, hostname string) (string, error) {
	if m.findZoneIDErr != nil {
		return "", m.findZoneIDErr
	}
	if m.zones != nil {
		if zoneID, ok := m.zones[hostname]; ok {
			return zoneID, nil
		}
	}
	return "test-zone-id", nil
}

func (m *mockCloudflareClient) EnsureDNSCNAME(_ context.Context, zoneID, hostname, target string) error {
	if m.ensureDNSErr != nil {
		return m.ensureDNSErr
	}
	m.ensureDNSCalls = append(m.ensureDNSCalls, mockDNSCall{ZoneID: zoneID, Hostname: hostname, Target: target})
	return nil
}

func (m *mockCloudflareClient) DeleteDNSCNAME(_ context.Context, zoneID, hostname string) error {
	if m.deleteDNSErr != nil {
		return m.deleteDNSErr
	}
	m.deleteDNSCalls = append(m.deleteDNSCalls, mockDNSCall{ZoneID: zoneID, Hostname: hostname})
	return nil
}

func (m *mockCloudflareClient) ListDNSCNAMEsByTarget(_ context.Context, _, _ string) ([]string, error) {
	if m.listDNSCNAMEsByTargetErr != nil {
		return nil, m.listDNSCNAMEsByTargetErr
	}
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
				Namespace: new(gatewayv1.Namespace(secretNamespace)),
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
	key := ctrlclient.ObjectKeyFromObject(gc)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.GatewayClass
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		ready := conditions.Find(result.Status.Conditions, apiv1.ConditionReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

func waitForGatewayProgrammed(g Gomega, gw *gatewayv1.Gateway) {
	key := ctrlclient.ObjectKeyFromObject(gw)
	g.Eventually(func(g Gomega) {
		var result gatewayv1.Gateway
		g.Expect(testClient.Get(testCtx, key, &result)).To(Succeed())
		programmed := conditions.Find(result.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
		g.Expect(programmed).NotTo(BeNil())
		g.Expect(programmed.Status).To(Equal(metav1.ConditionTrue))
	}).WithTimeout(10 * time.Second).WithPolling(100 * time.Millisecond).Should(Succeed())
}

// findEvent returns the first events.k8s.io/v1 Event for the given object name
// matching the specified type, reason, and action. Pass "" for action to match
// any action.
func findEvent(g Gomega, namespace, objectName, eventType, reason, action, noteSubstring string) *eventsv1.Event {
	var events eventsv1.EventList
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	g.Expect(testClient.List(testCtx, &events, opts...)).To(Succeed())
	for i, e := range events.Items {
		if e.Regarding.Name == objectName && e.Type == eventType && e.Reason == reason {
			if action == "" || e.Action == action {
				if noteSubstring == "" || strings.Contains(e.Note, noteSubstring) {
					return &events.Items[i]
				}
			}
		}
	}
	return nil
}

// resetMockErrors resets all error injection fields and call-tracking state
// on the shared mock at test cleanup. Tests that set mock errors MUST call
// this to avoid leaking error state to subsequent tests.
func resetMockErrors(t *testing.T) {
	t.Cleanup(func() {
		testMock.getTunnelIDByNameErr = nil
		testMock.createTunnelErr = nil
		testMock.deleteTunnelErr = nil
		testMock.getTunnelTokenErr = nil
		testMock.getTunnelConfigurationErr = nil
		testMock.updateTunnelConfigErr = nil
		testMock.listZoneIDsErr = nil
		testMock.findZoneIDErr = nil
		testMock.ensureDNSErr = nil
		testMock.deleteDNSErr = nil
		testMock.listDNSCNAMEsByTargetErr = nil
		testMock.deleteCalled = false
		testMock.deletedID = ""
	})
}
