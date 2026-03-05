// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package proxy

import (
	"github.com/rs/zerolog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

// ConfigMapWatcher watches a single ConfigMap via a Kubernetes informer and atomically
// updates the Proxy's routing table on add/update events.
type ConfigMapWatcher struct {
	factory informers.SharedInformerFactory
}

// NewConfigMapWatcher creates a ConfigMapWatcher that monitors the named ConfigMap in the given
// namespace and updates the proxy's config whenever the ConfigMap changes.
func NewConfigMapWatcher(clientset kubernetes.Interface, namespace, configMapName, configMapKey string, proxy *Proxy, zlog *zerolog.Logger) *ConfigMapWatcher {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = "metadata.name=" + configMapName
		}),
	)

	handler := func(obj any) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
		data, ok := cm.Data[configMapKey]
		if !ok {
			return
		}
		var cfg Config
		if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
			zlog.Error().Err(err).Msg("Failed to parse route config")
			return
		}
		if err := cfg.Parse(); err != nil {
			zlog.Error().Err(err).Msg("Invalid route config")
			return
		}
		proxy.SetConfig(&cfg)
	}

	informer := factory.Core().V1().ConfigMaps().Informer()
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handler,
		UpdateFunc: func(_, newObj any) { handler(newObj) },
	})

	return &ConfigMapWatcher{factory: factory}
}

// Start launches the informer goroutines and waits for the initial cache sync.
// It returns after the cache is synced; the informer continues running in the
// background until stopCh is closed.
func (w *ConfigMapWatcher) Start(stopCh <-chan struct{}) {
	w.factory.Start(stopCh)
	w.factory.WaitForCacheSync(stopCh)
}
