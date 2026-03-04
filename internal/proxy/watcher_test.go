// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/proxy"
)

func TestWatcher_LoadsConfigFromConfigMap(t *testing.T) {
	g := NewWithT(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("watcher-ok"))
	}))
	defer backend.Close()

	configData := `routes:
- hostname: app.example.com
  backends:
  - service: ` + backend.URL + `
    weight: 1
`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxy-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"config.yaml": configData,
		},
	}

	clientset := fake.NewClientset(cm)
	p := &proxy.Proxy{}
	watcher := proxy.NewWatcher(clientset, "default", "test-proxy-config", "config.yaml", p)

	stopCh := make(chan struct{})
	defer close(stopCh)
	go watcher.Start(stopCh)

	// Wait for the config to be loaded from the ConfigMap.
	g.Eventually(func(g Gomega) {
		req := httptest.NewRequest("GET", "http://app.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		g.Expect(rec.Code).To(Equal(http.StatusOK))
		g.Expect(rec.Body.String()).To(Equal("watcher-ok"))
	}).WithTimeout(5 * time.Second).WithPolling(50 * time.Millisecond).Should(Succeed())
}

func TestWatcher_IgnoresMissingKey(t *testing.T) {
	g := NewWithT(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxy-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"other-key.yaml": "routes: []",
		},
	}

	clientset := fake.NewClientset(cm)
	p := &proxy.Proxy{}
	watcher := proxy.NewWatcher(clientset, "default", "test-proxy-config", "config.yaml", p)

	stopCh := make(chan struct{})
	defer close(stopCh)
	go watcher.Start(stopCh)

	// Give the informer time to process.
	time.Sleep(200 * time.Millisecond)

	// Config should not be loaded (missing key), so proxy returns 503.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
}

func TestWatcher_IgnoresInvalidYAML(t *testing.T) {
	g := NewWithT(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxy-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"config.yaml": "not: [valid: yaml",
		},
	}

	clientset := fake.NewClientset(cm)
	p := &proxy.Proxy{}
	watcher := proxy.NewWatcher(clientset, "default", "test-proxy-config", "config.yaml", p)

	stopCh := make(chan struct{})
	defer close(stopCh)
	go watcher.Start(stopCh)

	// Give the informer time to process.
	time.Sleep(200 * time.Millisecond)

	// Config should not be loaded (invalid YAML), so proxy returns 503.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
}

func TestWatcher_IgnoresInvalidServiceURL(t *testing.T) {
	g := NewWithT(t)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-proxy-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"config.yaml": `routes:
- hostname: app.example.com
  backends:
  - service: "://invalid"
    weight: 1
`,
		},
	}

	clientset := fake.NewClientset(cm)
	p := &proxy.Proxy{}
	watcher := proxy.NewWatcher(clientset, "default", "test-proxy-config", "config.yaml", p)

	stopCh := make(chan struct{})
	defer close(stopCh)
	go watcher.Start(stopCh)

	// Give the informer time to process.
	time.Sleep(200 * time.Millisecond)

	// Config should not be loaded (invalid service URL), so proxy returns 503.
	req := httptest.NewRequest("GET", "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	g.Expect(rec.Code).To(Equal(http.StatusServiceUnavailable))
}
