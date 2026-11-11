// Package demo provides a small, made-up cluster so that the report can be
// tried without Kubernetes or Prometheus. The data flows through the same
// analysis and rendering code as real data.
package demo

import (
	"fmt"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/dcgm"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kube"
)

// Source is the report's "source" field in demo mode.
const Source = "bundled sample data (--demo)"

const gib = 1024.0

type workload struct {
	namespace, kind, name string
	replicas              int
	resource              string
	gpusPerPod            int
	modelName             string
	migProfile            string
	capacityMiB           float64
	peakMiB               float64
	avg, p95, max         float64
	// age is how long the pods have existed.
	age time.Duration
	// lastActive is how long ago utilization last exceeded the idle
	// threshold; negative means "active right now".
	lastActive time.Duration
	// noMetrics simulates a pod that DCGM knows nothing about.
	noMetrics bool
	// noUtil simulates a MIG instance without utilization metrics.
	noUtil bool
}

const day = 24 * time.Hour

var workloads = []workload{
	{namespace: "ml-serving", kind: "Deployment", name: "embed-api", replicas: 4, resource: kube.ResourceGPU, gpusPerPod: 1,
		modelName: "NVIDIA A100-SXM4-80GB", capacityMiB: 81920, peakMiB: 6.1 * gib, avg: 9, p95: 12, max: 31, age: 41 * day, lastActive: -1},
	{namespace: "ml-training", kind: "StatefulSet", name: "train-nightly", replicas: 1, resource: kube.ResourceGPU, gpusPerPod: 4,
		modelName: "NVIDIA H100 80GB HBM3", capacityMiB: 81559, peakMiB: 71 * gib, avg: 88, p95: 99, max: 100, age: 19 * day, lastActive: -1},
	{namespace: "notebooks", kind: "Pod", name: "notebook-priya", replicas: 1, resource: kube.ResourceGPU, gpusPerPod: 1,
		modelName: "NVIDIA A100-SXM4-80GB", capacityMiB: 81920, peakMiB: 0.3 * gib, avg: 0, p95: 0, max: 0, age: 23 * day, lastActive: 9*day + 5*time.Hour},
	{namespace: "ml-serving", kind: "Deployment", name: "llm-gateway", replicas: 2, resource: kube.ResourceGPU, gpusPerPod: 1,
		modelName: "NVIDIA H100 80GB HBM3", capacityMiB: 81559, peakMiB: 62 * gib, avg: 35, p95: 71, max: 96, age: 12 * day, lastActive: -1},
	{namespace: "ml-serving", kind: "Deployment", name: "rerank-api", replicas: 2, resource: kube.ResourceGPU, gpusPerPod: 1,
		modelName: "Tesla T4", capacityMiB: 15360, peakMiB: 3.2 * gib, avg: 6, p95: 14, max: 38, age: 30 * day, lastActive: -1},
	{namespace: "vision", kind: "Deployment", name: "ocr-worker", replicas: 2, resource: kube.ResourceGPU, gpusPerPod: 1,
		modelName: "NVIDIA A10G", capacityMiB: 23028, peakMiB: 13 * gib, avg: 12, p95: 22, max: 47, age: 16 * day, lastActive: -1},
	{namespace: "research", kind: "Job", name: "eval-sweep", replicas: 1, resource: kube.ResourceGPU, gpusPerPod: 2,
		modelName: "NVIDIA A100-SXM4-80GB", capacityMiB: 81920, peakMiB: 22 * gib, avg: 18, p95: 41, max: 77, age: 3 * day, lastActive: -1},
	{namespace: "ml-serving", kind: "Deployment", name: "tagger", replicas: 3, resource: kube.ResourceMIGPrefix + "3g.40gb", gpusPerPod: 1,
		modelName: "NVIDIA A100-SXM4-80GB", migProfile: "3g.40gb", capacityMiB: 40192, peakMiB: 17 * gib, age: 8 * day, lastActive: -1, noUtil: true},
	{namespace: "sandbox", kind: "Pod", name: "cuda-debug", replicas: 1, resource: kube.ResourceGPU, gpusPerPod: 1,
		age: 40 * time.Minute, noMetrics: true},
}

// Data returns the sample workloads and their usage as it would look when
// queried at now over the given window.
func Data(now time.Time, window time.Duration) ([]kube.Workload, []dcgm.Usage) {
	var ws []kube.Workload
	var usage []dcgm.Usage
	for _, w := range workloads {
		kw := kube.Workload{Namespace: w.namespace, Kind: w.kind, Name: w.name}
		for i := 0; i < w.replicas; i++ {
			pod := kube.Pod{
				Namespace: w.namespace,
				Name:      podName(w, i),
				Node:      fmt.Sprintf("gpu-node-%d", len(usage)%7),
				Created:   now.Add(-w.age),
				Request:   kube.Request{Resource: w.resource, Count: w.gpusPerPod},
			}
			kw.Pods = append(kw.Pods, pod)
			if w.noMetrics {
				continue
			}
			observed := w.age
			if observed > window {
				observed = window
			}
			for g := 0; g < w.gpusPerPod; g++ {
				// Spread replicas a little so they are not identical.
				jitter := 1 - 0.04*float64((i+g)%3)
				u := dcgm.Usage{
					Namespace:   w.namespace,
					Pod:         pod.Name,
					Container:   "main",
					Device:      fmt.Sprintf("GPU-demo-%s-%d-%d", w.name, i, g),
					ModelName:   w.modelName,
					MIGProfile:  w.migProfile,
					HasMemory:   true,
					MemPeakMiB:  w.peakMiB * jitter,
					MemAvgMiB:   w.peakMiB * jitter * 0.8,
					CapacityMiB: w.capacityMiB,
					Observed:    observed,
				}
				if !w.noUtil {
					u.HasUtil, u.HasRecent = true, true
					u.UtilAvg, u.UtilP95, u.UtilMax = w.avg*jitter, w.p95*jitter, w.max
					u.UtilRecentMax = w.max
					switch {
					case w.lastActive < 0:
						u.LastActive = now
					case w.lastActive < window:
						u.LastActive = now.Add(-w.lastActive)
						u.UtilRecentMax = 0
					default:
						// Last activity predates the window.
						u.UtilRecentMax = 0
					}
				}
				usage = append(usage, u)
			}
		}
		ws = append(ws, kw)
	}
	return ws, usage
}

func podName(w workload, i int) string {
	switch w.kind {
	case "Pod":
		return w.name
	case "StatefulSet":
		return fmt.Sprintf("%s-%d", w.name, i)
	case "Job":
		return fmt.Sprintf("%s-%s", w.name, []string{"x7k2p", "m4q9z", "c8v1n"}[i%3])
	default:
		return fmt.Sprintf("%s-6d5f8c7b9-%s", w.name, []string{"x7k2p", "m4q9z", "c8v1n", "t3h6w", "r5j0d"}[i%5])
	}
}
