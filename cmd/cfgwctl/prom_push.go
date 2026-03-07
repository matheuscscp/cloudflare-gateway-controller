// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// promPush gathers metrics from the registry and pushes them to the
// Grafana Cloud OTLP endpoint. Set PROM_PUSH_TOKEN to a value in the
// format "instance_id:api_key" to enable (Basic Auth).
func promPush(registry *prometheus.Registry, startTime time.Time) error {
	token := os.Getenv("PROM_PUSH_TOKEN")
	if token == "" {
		return nil
	}
	username, password, _ := strings.Cut(token, ":")

	mfs, err := registry.Gather()
	if err != nil {
		return fmt.Errorf("gathering metrics: %w", err)
	}

	now := time.Now()
	extra := promExtraLabels()

	payload := otlpExport{
		ResourceMetrics: []otlpResourceMetrics{{
			ScopeMetrics: []otlpScopeMetrics{{
				Metrics: mfsToOTLP(mfs, startTime, now, extra),
			}},
		}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling OTLP payload: %w", err)
	}

	const otlpURL = "https://otlp-gateway-prod-eu-west-6.grafana.net/otlp/v1/metrics"
	req, err := http.NewRequest("POST", otlpURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(username, password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OTLP push returned %d: %s", resp.StatusCode, respBody)
	}

	fmt.Println("\nMetrics pushed to Grafana Cloud OTLP endpoint")
	return nil
}

func promExtraLabels() []otlpAttribute {
	job := os.Getenv("PROM_PUSH_JOB")
	if job == "" {
		job = "cfgw_e2e_load"
	}
	attrs := []otlpAttribute{{Key: "job", Value: otlpAttrValue{StringValue: job}}}
	if runURL := os.Getenv("PROM_PUSH_RUN_URL"); runURL != "" {
		attrs = append(attrs, otlpAttribute{
			Key:   "run_url",
			Value: otlpAttrValue{StringValue: runURL},
		})
	}
	return attrs
}

func mfsToOTLP(mfs []*dto.MetricFamily, start, end time.Time, extra []otlpAttribute) []otlpMetric {
	startNano := strconv.FormatUint(uint64(start.UnixNano()), 10)
	endNano := strconv.FormatUint(uint64(end.UnixNano()), 10)

	var metrics []otlpMetric
	for _, mf := range mfs {
		if mf.GetType() != dto.MetricType_HISTOGRAM {
			continue
		}

		var dataPoints []otlpHistogramDataPoint
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()

			// Build attributes from metric labels + extra labels.
			var attrs []otlpAttribute
			for _, lp := range m.GetLabel() {
				attrs = append(attrs, otlpAttribute{
					Key:   lp.GetName(),
					Value: otlpAttrValue{StringValue: lp.GetValue()},
				})
			}
			attrs = append(attrs, extra...)

			// Convert Prometheus cumulative buckets to OTLP delta buckets.
			promBuckets := h.GetBucket()
			bounds := make([]float64, len(promBuckets))
			bucketCounts := make([]string, len(promBuckets)+1)
			var prev uint64
			for i, b := range promBuckets {
				bounds[i] = b.GetUpperBound()
				bucketCounts[i] = strconv.FormatUint(b.GetCumulativeCount()-prev, 10)
				prev = b.GetCumulativeCount()
			}
			// Overflow bucket: total count minus last cumulative.
			bucketCounts[len(promBuckets)] = strconv.FormatUint(h.GetSampleCount()-prev, 10)

			dataPoints = append(dataPoints, otlpHistogramDataPoint{
				StartTimeUnixNano: startNano,
				TimeUnixNano:      endNano,
				Count:             strconv.FormatUint(h.GetSampleCount(), 10),
				Sum:               h.GetSampleSum(),
				BucketCounts:      bucketCounts,
				ExplicitBounds:    bounds,
				Attributes:        attrs,
			})
		}

		metrics = append(metrics, otlpMetric{
			Name:        mf.GetName(),
			Unit:        "s",
			Description: mf.GetHelp(),
			Histogram: &otlpHistogram{
				AggregationTemporality: 2, // CUMULATIVE
				DataPoints:             dataPoints,
			},
		})
	}
	return metrics
}

// OTLP JSON types.

type otlpExport struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Metrics []otlpMetric `json:"metrics"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Unit        string         `json:"unit"`
	Description string         `json:"description"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

type otlpHistogram struct {
	AggregationTemporality int                      `json:"aggregationTemporality"`
	DataPoints             []otlpHistogramDataPoint `json:"dataPoints"`
}

type otlpHistogramDataPoint struct {
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	Count             string          `json:"count"`
	Sum               float64         `json:"sum"`
	BucketCounts      []string        `json:"bucketCounts"`
	ExplicitBounds    []float64       `json:"explicitBounds"`
	Attributes        []otlpAttribute `json:"attributes"`
}

type otlpAttribute struct {
	Key   string        `json:"key"`
	Value otlpAttrValue `json:"value"`
}

type otlpAttrValue struct {
	StringValue string `json:"stringValue"`
}
