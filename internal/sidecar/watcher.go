// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package sidecar

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

// Watcher watches a single ConfigMap via a Kubernetes informer and atomically
// updates the Proxy's routing table on add/update events.
type Watcher struct {
	factory informers.SharedInformerFactory
}

// NewWatcher creates a Watcher that monitors the named ConfigMap in the given
// namespace and updates the proxy's config whenever the ConfigMap changes.
func NewWatcher(clientset kubernetes.Interface, namespace, configMapName, configMapKey string, proxy *Proxy) *Watcher {
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
			fmt.Printf("sidecar watcher: failed to parse config: %v\n", err)
			return
		}
		if err := cfg.ParseServiceURLs(); err != nil {
			fmt.Printf("sidecar watcher: invalid config: %v\n", err)
			return
		}
		proxy.SetConfig(&cfg)
	}

	informer := factory.Core().V1().ConfigMaps().Informer()
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handler,
		UpdateFunc: func(_, newObj any) { handler(newObj) },
	})

	return &Watcher{factory: factory}
}

// Start starts the informer and blocks until stopCh is closed.
func (w *Watcher) Start(stopCh <-chan struct{}) {
	w.factory.Start(stopCh)
	w.factory.WaitForCacheSync(stopCh)
}
