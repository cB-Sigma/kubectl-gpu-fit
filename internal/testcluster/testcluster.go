// Package testcluster describes a small imaginary GPU cluster: Kubernetes
// objects plus matching DCGM series. Tests and the hack/fake-cluster tool
// share it.
package testcluster

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kt "github.com/cb-Sigma/kubectl-gpu-fit/internal/kubetest"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/promtest"
)

const (
	// PrometheusNamespace is the namespace of the Prometheus Service that
	// discovery is expected to find.
	PrometheusNamespace = "monitoring"
	// PrometheusService is the name of that Service.
	PrometheusService = "kube-prometheus-stack-prometheus"

	gpuResource = "nvidia.com/gpu"
)

// WantTable is the report expected for the cluster across all namespaces
// with default settings.
const WantTable = `NAMESPACE     WORKLOAD         REQUESTED   PEAK MEM   AVG UTIL   RECOMMENDATION         WASTE/MO
ml-serving    embed-api (x4)   1x A100     6.1 GB     12%        MIG 1g.10gb slice      $13,000
notebooks     notebook-priya   1x A100     0.3 GB     0%         idle 5 days, reclaim   $3,700
ml-training   train-nightly    4x H100     71 GB      88%        keep as is             -
sandbox       fresh            1x A100     0.5 GB     1%         not enough data        -

4 workloads, 10 GPUs, est. cost $51,000/mo, est. waste $16,000/mo (32%)
Window 7d, memory headroom 20%, all namespaces. Costs are estimates from rough on-demand list prices per GPU-hour; override with --price or --price-file.
`

func dcgmLabels(ns, pod, uuid, model string, exported bool) map[string]string {
	l := map[string]string{"gpu": "0", "UUID": uuid, "device": "nvidia0", "modelName": model, "Hostname": "node-1"}
	if exported {
		// Scraped without honorLabels: target labels win, the workload
		// moves to exported_*.
		l["pod"], l["namespace"], l["container"] = "nvidia-dcgm-exporter-abcde", "gpu-operator", "nvidia-dcgm-exporter"
		l["exported_pod"], l["exported_namespace"], l["exported_container"] = pod, ns, "main"
	} else {
		l["pod"], l["namespace"], l["container"] = pod, ns, "main"
	}
	return l
}

type gpuSeries struct {
	ns, pod, uuid, model   string
	avg, p95, max, recent  float64
	lastActive             time.Time
	peakMiB, avgMiB, total float64
	samples                float64
}

func (g gpuSeries) series(exported bool) []promtest.Series {
	l := dcgmLabels(g.ns, g.pod, g.uuid, g.model, exported)
	return []promtest.Series{
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: l, Avg: g.avg, P95: g.p95, Max: g.max, RecentMax: g.recent, LastActive: g.lastActive},
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: l, Avg: g.avgMiB, Max: g.peakMiB, Samples: g.samples},
		{Metric: "DCGM_FI_DEV_FB_FREE", Labels: l, Avg: g.total - g.avgMiB},
	}
}

// New returns the cluster's objects and DCGM series as of now. With
// exportedLabels the series look as if scraped without honorLabels.
func New(now time.Time, exportedLabels bool) ([]runtime.Object, []promtest.Series) {
	const a100, h100 = "NVIDIA A100-SXM4-80GB", "NVIDIA H100 80GB HBM3"
	const fullWeek = 2016 // 7d at 5m
	var series []promtest.Series
	add := func(g gpuSeries) { series = append(series, g.series(exportedLabels)...) }

	objs := []runtime.Object{
		kt.ReplicaSet("ml-serving", "embed-api-5d8f7c9b6", "embed-api"),
		kt.Pod("notebooks", "notebook-priya", gpuResource, 1),
		kt.OwnedBy(kt.Pod("ml-training", "train-nightly-0", gpuResource, 4), "StatefulSet", "train-nightly"),
		kt.Pod("sandbox", "fresh", gpuResource, 1),
		kt.Pod("sandbox", "cpu-only", "", 0),
		kt.Node("node-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB", "nvidia.com/gpu.memory": "81920"}),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: PrometheusNamespace, Name: PrometheusService, Labels: map[string]string{"app.kubernetes.io/name": "prometheus"}},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http-web", Port: 9090}}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: PrometheusNamespace, Name: "kube-prometheus-stack-alertmanager"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http-web", Port: 9093}}},
		},
	}
	for i, suffix := range []string{"aaaaa", "bbbbb", "ccccc", "ddddd"} {
		name := "embed-api-5d8f7c9b6-" + suffix
		objs = append(objs, kt.OwnedBy(kt.Pod("ml-serving", name, gpuResource, 1), "ReplicaSet", "embed-api-5d8f7c9b6"))
		add(gpuSeries{ns: "ml-serving", pod: name, uuid: "GPU-embed-" + suffix, model: a100,
			avg: 10 + float64(i), p95: 12, max: 35, recent: 30, lastActive: now,
			peakMiB: 6246 - 100*float64(i), avgMiB: 5000, total: 81920, samples: fullWeek})
	}
	add(gpuSeries{ns: "notebooks", pod: "notebook-priya", uuid: "GPU-nb", model: a100,
		avg: 0.2, p95: 0, max: 61, recent: 0, lastActive: now.Add(-5*24*time.Hour - time.Hour),
		peakMiB: 307, avgMiB: 300, total: 81920, samples: fullWeek})
	for g := 0; g < 4; g++ {
		add(gpuSeries{ns: "ml-training", pod: "train-nightly-0", uuid: fmt.Sprintf("GPU-train-%d", g), model: h100,
			avg: 88, p95: 99, max: 100, recent: 100, lastActive: now,
			peakMiB: 72704, avgMiB: 60000, total: 81559, samples: fullWeek})
	}
	// "fresh" has only ten minutes of data.
	add(gpuSeries{ns: "sandbox", pod: "fresh", uuid: "GPU-fresh", model: a100,
		avg: 1, p95: 1, max: 1, recent: 1, peakMiB: 500, avgMiB: 400, total: 81920, samples: 2})
	// A GPU nobody uses, and a pod that no longer exists.
	series = append(series, promtest.Series{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: map[string]string{"gpu": "7", "UUID": "GPU-free", "modelName": a100}})
	add(gpuSeries{ns: "ml-serving", pod: "embed-api-oldrs-zzzzz", uuid: "GPU-gone", model: a100, avg: 99, p95: 99, max: 99, peakMiB: 80000, avgMiB: 80000, total: 81920, samples: 10})
	return objs, series
}

// Kubeconfig returns a kubeconfig for an unauthenticated API server at the
// given URL, with the context's namespace set.
func Kubeconfig(server, namespace string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
users:
- name: fake
  user: {}
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
    namespace: %s
current-context: fake
`, server, namespace)
}
