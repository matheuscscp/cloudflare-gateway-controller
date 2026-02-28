// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func newTestLoadCmd() *cobra.Command {
	var (
		url           string
		requests      int
		concurrency   int
		namespace     string
		labelSelector string
		podPort       int
		maxCV         float64
		kubeconfig    string
	)

	cmd := &cobra.Command{
		Use:   "load",
		Short: "Generate HTTP load and optionally check pod distribution",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Phase 1: Generate load.
			fmt.Printf("Sending %d requests to %s with concurrency %d...\n", requests, url, concurrency)

			var (
				ok2xx  atomic.Int64
				err5xx atomic.Int64
				other  atomic.Int64
			)

			work := make(chan struct{}, requests)
			for range requests {
				work <- struct{}{}
			}
			close(work)

			var wg sync.WaitGroup
			for range concurrency {
				wg.Go(func() {
					for range work {
						resp, err := http.Get(url)
						if err != nil {
							other.Add(1)
							continue
						}
						_ = resp.Body.Close()
						switch {
						case resp.StatusCode >= 200 && resp.StatusCode < 300:
							ok2xx.Add(1)
						case resp.StatusCode >= 500:
							err5xx.Add(1)
						default:
							other.Add(1)
						}
					}
				})
			}
			wg.Wait()

			fmt.Printf("\nResults:\n")
			fmt.Printf("  2xx: %d\n", ok2xx.Load())
			fmt.Printf("  5xx: %d\n", err5xx.Load())
			fmt.Printf("  other/errors: %d\n", other.Load())

			if got := ok2xx.Load(); got != int64(requests) {
				return fmt.Errorf("expected %d 2xx responses, got %d (5xx: %d, other: %d)",
					requests, got, err5xx.Load(), other.Load())
			}

			// Phase 2: Pod distribution check (optional).
			if namespace == "" || labelSelector == "" {
				fmt.Println("\nSkipping pod distribution check (no --namespace/--label-selector)")
				return nil
			}

			fmt.Printf("\nChecking pod distribution (namespace=%s, selector=%s)...\n", namespace, labelSelector)

			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			if kubeconfig != "" {
				loadingRules.ExplicitPath = kubeconfig
			}
			config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				loadingRules, &clientcmd.ConfigOverrides{},
			).ClientConfig()
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("creating kubernetes client: %w", err)
			}

			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				return fmt.Errorf("listing pods: %w", err)
			}

			if len(pods.Items) == 0 {
				return fmt.Errorf("no pods found matching selector %q in namespace %q", labelSelector, namespace)
			}

			type podCount struct {
				Name  string
				Count int64
			}
			var counts []podCount

			for _, pod := range pods.Items {
				count, err := getPodCount(ctx, clientset, namespace, pod.Name, podPort)
				if err != nil {
					return fmt.Errorf("getting count from pod %s: %w", pod.Name, err)
				}
				counts = append(counts, podCount{Name: pod.Name, Count: count})
			}

			fmt.Printf("\nPer-pod request counts:\n")
			var sum int64
			for _, pc := range counts {
				fmt.Printf("  %s: %d\n", pc.Name, pc.Count)
				sum += pc.Count
			}

			mean := float64(sum) / float64(len(counts))
			var varianceSum float64
			for _, pc := range counts {
				diff := float64(pc.Count) - mean
				varianceSum += diff * diff
			}
			stddev := math.Sqrt(varianceSum / float64(len(counts)))
			cv := stddev / mean

			fmt.Printf("\nDistribution stats:\n")
			fmt.Printf("  total: %d\n", sum)
			fmt.Printf("  mean: %.1f\n", mean)
			fmt.Printf("  stddev: %.1f\n", stddev)
			fmt.Printf("  CV (stddev/mean): %.3f (max: %.3f)\n", cv, maxCV)

			for _, pc := range counts {
				if pc.Count == 0 {
					return fmt.Errorf("pod %s received 0 requests", pc.Name)
				}
			}

			if cv > maxCV {
				return fmt.Errorf("coefficient of variation %.3f exceeds max %.3f", cv, maxCV)
			}

			fmt.Println("\nDistribution check PASSED")
			return nil
		},
	}

	cmd.Flags().StringVar(&url, "url", "", "target URL (required)")
	cmd.Flags().IntVar(&requests, "requests", 1000, "total requests to send")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "concurrent workers")
	cmd.Flags().StringVar(&namespace, "namespace", "", "pod namespace for distribution check")
	cmd.Flags().StringVar(&labelSelector, "label-selector", "", "pod label selector for distribution check")
	cmd.Flags().IntVar(&podPort, "pod-port", 8080, "pod port for /_count endpoint")
	cmd.Flags().Float64Var(&maxCV, "max-cv", 0.5, "max coefficient of variation")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig path (optional)")
	cobra.CheckErr(cmd.MarkFlagRequired("url"))

	return cmd
}

func getPodCount(ctx context.Context, clientset kubernetes.Interface, namespace, podName string, port int) (int64, error) {
	resp := clientset.CoreV1().RESTClient().
		Get().
		Namespace(namespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", podName, port)).
		SubResource("proxy").
		Suffix("_count").
		Do(ctx)

	raw, err := resp.Raw()
	if err != nil {
		return 0, fmt.Errorf("proxy request: %w", err)
	}

	var result struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("parsing response: %w", err)
	}

	return result.Count, nil
}
