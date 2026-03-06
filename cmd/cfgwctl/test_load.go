// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func newTestLoadCmd() *cobra.Command {
	var (
		url            string
		requests       int
		duration       time.Duration
		concurrency    int
		namespace      string
		labelSelector  string
		podPort        int
		maxCV          float64
		kubeconfig     string
		backends       []string
		tolerance      float64
		hostname       string
		minSuccessRate float64
	)

	cmd := &cobra.Command{
		Use:   "load",
		Short: "Generate HTTP load and optionally check pod distribution",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Phase 1: Generate load.
			var (
				ok2xx    atomic.Int64
				err5xx   atomic.Int64
				other    atomic.Int64
				hostErrs atomic.Int64
				total    atomic.Int64
			)

			sendOne := func() {
				total.Add(1)
				resp, err := http.Get(url)
				if err != nil {
					other.Add(1)
					return
				}
				if hostname != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var body struct {
						Host string `json:"host"`
					}
					if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
						if body.Host != hostname {
							hostErrs.Add(1)
						}
					}
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

			if duration > 0 {
				fmt.Printf("Sending requests to %s with concurrency %d for %s...\n", url, concurrency, duration)
				ctx, cancel := context.WithTimeout(ctx, duration)
				defer cancel()
				var wg sync.WaitGroup
				for range concurrency {
					wg.Go(func() {
						for {
							select {
							case <-ctx.Done():
								return
							default:
								sendOne()
							}
						}
					})
				}
				wg.Wait()
			} else {
				fmt.Printf("Sending %d requests to %s with concurrency %d...\n", requests, url, concurrency)
				work := make(chan struct{}, requests)
				for range requests {
					work <- struct{}{}
				}
				close(work)
				var wg sync.WaitGroup
				for range concurrency {
					wg.Go(func() {
						for range work {
							sendOne()
						}
					})
				}
				wg.Wait()
			}

			totalSent := total.Load()
			fmt.Printf("\nResults:\n")
			fmt.Printf("  total: %d\n", totalSent)
			fmt.Printf("  2xx: %d\n", ok2xx.Load())
			fmt.Printf("  5xx: %d\n", err5xx.Load())
			fmt.Printf("  other/errors: %d\n", other.Load())
			if hostname != "" {
				fmt.Printf("  host mismatches: %d\n", hostErrs.Load())
			}

			successRate := float64(ok2xx.Load()) / float64(totalSent)
			fmt.Printf("  success rate: %.5f%% (min: %.5f%%)\n", successRate*100, minSuccessRate*100)
			if successRate < minSuccessRate {
				return fmt.Errorf("success rate %.5f%% below minimum %.5f%% (5xx: %d, other: %d, 2xx: %d/%d)",
					successRate*100, minSuccessRate*100, err5xx.Load(), other.Load(), ok2xx.Load(), totalSent)
			}

			if hostname != "" && hostErrs.Load() > 0 {
				return fmt.Errorf("%d responses had incorrect host (expected %q)", hostErrs.Load(), hostname)
			}

			// Phase 2a: Weighted backend distribution check (optional).
			if len(backends) > 0 {
				return checkWeightedDistribution(ctx, namespace, backends, podPort, tolerance, kubeconfig)
			}

			// Phase 2b: Even pod distribution check (optional).
			if namespace == "" || labelSelector == "" {
				fmt.Println("\nSkipping pod distribution check (no --namespace/--label-selector)")
				return nil
			}

			fmt.Printf("\nChecking pod distribution (namespace=%s, selector=%s)...\n", namespace, labelSelector)

			clientset, err := buildClientset(kubeconfig)
			if err != nil {
				return err
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
	cmd.Flags().IntVar(&requests, "requests", 1000, "total requests to send (ignored when --duration is set)")
	cmd.Flags().DurationVar(&duration, "duration", 0, "send requests continuously for this duration (e.g. 1m)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 10, "concurrent workers")
	cmd.Flags().StringVar(&namespace, "namespace", "", "pod namespace for distribution check")
	cmd.Flags().StringVar(&labelSelector, "label-selector", "", "pod label selector for distribution check")
	cmd.Flags().IntVar(&podPort, "pod-port", 8080, "pod port for /_count endpoint")
	cmd.Flags().Float64Var(&maxCV, "max-cv", 0.5, "max coefficient of variation")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig path (optional)")
	cmd.Flags().StringArrayVar(&backends, "backend", nil,
		"weighted backend for traffic splitting check: selector:weight (e.g. app=svc-a:80); repeatable")
	cmd.Flags().Float64Var(&tolerance, "tolerance", 0.15,
		"max deviation from expected share (0.15 = ±15%) for --backend checks")
	cmd.Flags().StringVar(&hostname, "hostname", "", "expected Host header value in responses (optional)")
	cmd.Flags().Float64Var(&minSuccessRate, "min-success-rate", 1.0, "minimum required success rate (e.g. 0.99999 for 5 nines)")
	cobra.CheckErr(cmd.MarkFlagRequired("url"))

	return cmd
}

// backendSpec holds a parsed --backend flag value.
type backendSpec struct {
	selector string
	weight   int
}

// parseBackendFlag parses "selector:weight" (e.g. "app=svc-a:80").
func parseBackendFlag(s string) (backendSpec, error) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return backendSpec{}, fmt.Errorf("invalid --backend %q: expected selector:weight", s)
	}
	sel := s[:idx]
	w, err := strconv.Atoi(s[idx+1:])
	if err != nil {
		return backendSpec{}, fmt.Errorf("invalid weight in --backend %q: %w", s, err)
	}
	if w < 0 {
		return backendSpec{}, fmt.Errorf("negative weight in --backend %q", s)
	}
	return backendSpec{selector: sel, weight: w}, nil
}

// checkWeightedDistribution verifies that traffic was split across backend
// groups according to the specified weights within the given tolerance.
func checkWeightedDistribution(ctx context.Context, namespace string, backendFlags []string, podPort int, tolerance float64, kubeconfig string) error {
	if namespace == "" {
		return fmt.Errorf("--namespace is required when using --backend")
	}

	specs := make([]backendSpec, 0, len(backendFlags))
	var totalWeight int
	for _, b := range backendFlags {
		spec, err := parseBackendFlag(b)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
		totalWeight += spec.weight
	}
	if totalWeight == 0 {
		return fmt.Errorf("total weight must be > 0")
	}

	clientset, err := buildClientset(kubeconfig)
	if err != nil {
		return err
	}

	fmt.Printf("\nChecking weighted backend distribution (namespace=%s)...\n", namespace)

	type groupResult struct {
		selector      string
		weight        int
		count         int64
		expectedShare float64
		actualShare   float64
	}

	var totalCount int64
	results := make([]groupResult, len(specs))
	for i, spec := range specs {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: spec.selector,
		})
		if err != nil {
			return fmt.Errorf("listing pods for selector %q: %w", spec.selector, err)
		}
		if len(pods.Items) == 0 {
			return fmt.Errorf("no pods found matching selector %q in namespace %q", spec.selector, namespace)
		}

		var groupCount int64
		for _, pod := range pods.Items {
			count, err := getPodCount(ctx, clientset, namespace, pod.Name, podPort)
			if err != nil {
				return fmt.Errorf("getting count from pod %s: %w", pod.Name, err)
			}
			groupCount += count
		}
		results[i] = groupResult{
			selector:      spec.selector,
			weight:        spec.weight,
			count:         groupCount,
			expectedShare: float64(spec.weight) / float64(totalWeight),
		}
		totalCount += groupCount
	}

	fmt.Printf("\nPer-backend request counts:\n")
	for i := range results {
		results[i].actualShare = float64(results[i].count) / float64(totalCount)
		fmt.Printf("  %s (weight %d): %d requests (%.1f%% actual, %.1f%% expected)\n",
			results[i].selector, results[i].weight, results[i].count,
			results[i].actualShare*100, results[i].expectedShare*100)
	}
	fmt.Printf("  total: %d\n", totalCount)

	// Verify each group received traffic and is within tolerance.
	for _, r := range results {
		if r.weight > 0 && r.count == 0 {
			return fmt.Errorf("backend %q (weight %d) received 0 requests", r.selector, r.weight)
		}
		deviation := math.Abs(r.actualShare - r.expectedShare)
		if deviation > tolerance {
			return fmt.Errorf("backend %q: actual share %.1f%% deviates %.1f%% from expected %.1f%% (max tolerance %.1f%%)",
				r.selector, r.actualShare*100, deviation*100, r.expectedShare*100, tolerance*100)
		}
	}

	fmt.Println("\nWeighted distribution check PASSED")
	return nil
}

func buildClientset(kubeconfig string) (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}
	return clientset, nil
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
