package dcgm_test

import (
	"context"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/dcgm"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/prom"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/promtest"
)

var now = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

const mib = 1 << 20

func collect(t *testing.T, series []promtest.Series, mutate func(*dcgm.Options)) ([]dcgm.Usage, dcgm.Stats, *promtest.Server) {
	t.Helper()
	srv := promtest.New("7d", series)
	t.Cleanup(srv.Close)
	tr, err := prom.NewHTTPTransport(srv.URL, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := dcgm.Options{Window: 7 * 24 * time.Hour, Recent: 24 * time.Hour, Step: 5 * time.Minute, IdleUtil: 5, Now: now}
	if mutate != nil {
		mutate(&opts)
	}
	usage, stats, err := dcgm.Collect(context.Background(), prom.NewClient(tr), opts)
	if err != nil {
		t.Fatal(err)
	}
	return usage, stats, srv
}

func labels(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Classic dcgm-exporter: percent and MiB, pod/namespace/container labels
// preserved by honorLabels.
func TestCollectLegacyMetrics(t *testing.T) {
	gpu := labels("gpu", "0", "UUID", "GPU-aaa", "device", "nvidia0", "modelName", "NVIDIA A100-SXM4-80GB", "Hostname", "node-1",
		"pod", "embed-1", "namespace", "ml", "container", "server")
	free := labels("gpu", "3", "UUID", "GPU-free", "modelName", "NVIDIA A100-SXM4-80GB", "Hostname", "node-1")
	lastActive := now.Add(-3 * time.Hour)
	usage, stats, srv := collect(t, []promtest.Series{
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: gpu, Avg: 11, Max: 64, P95: 23, RecentMax: 40, LastActive: lastActive},
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: gpu, Avg: 5000, Max: 6246, Samples: 2016},
		{Metric: "DCGM_FI_DEV_FB_FREE", Labels: gpu, Avg: 75920},
		{Metric: "DCGM_FI_DEV_FB_RESERVED", Labels: gpu, Avg: 1000},
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: free, Avg: 0},
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: free, Avg: 0, Samples: 2016},
	}, nil)

	if len(usage) != 1 {
		t.Fatalf("want 1 attributed device, got %+v", usage)
	}
	u := usage[0]
	if u.Namespace != "ml" || u.Pod != "embed-1" || u.Container != "server" || u.Device != "GPU-aaa" || u.Host != "node-1" {
		t.Errorf("identity = %+v", u)
	}
	if u.ModelName != "NVIDIA A100-SXM4-80GB" || u.MIGProfile != "" {
		t.Errorf("model = %q / %q", u.ModelName, u.MIGProfile)
	}
	if !u.HasUtil || u.UtilAvg != 11 || u.UtilMax != 64 || u.UtilP95 != 23 || !u.HasRecent || u.UtilRecentMax != 40 {
		t.Errorf("utilization = %+v", u)
	}
	if !u.LastActive.Equal(lastActive) {
		t.Errorf("LastActive = %v, want %v", u.LastActive, lastActive)
	}
	if !u.HasMemory || u.MemPeakMiB != 6246 || u.MemAvgMiB != 5000 {
		t.Errorf("memory = %+v", u)
	}
	if u.CapacityMiB != 81920 { // avg used + avg free + avg reserved
		t.Errorf("CapacityMiB = %v, want 81920", u.CapacityMiB)
	}
	if u.Observed != 7*24*time.Hour {
		t.Errorf("Observed = %v", u.Observed)
	}
	if stats.Unattributed == 0 || stats.Series <= stats.Unattributed {
		t.Errorf("stats = %+v", stats)
	}
	wantMetrics := "DCGM_FI_DEV_FB_FREE,DCGM_FI_DEV_FB_RESERVED,DCGM_FI_DEV_FB_USED,DCGM_FI_DEV_GPU_UTIL"
	if got := strings.Join(stats.Metrics, ","); got != wantMetrics {
		t.Errorf("metrics seen = %s", got)
	}

	queries := strings.Join(srv.Queries(), "\n")
	for _, want := range []string{
		"avg_over_time(DCGM_FI_DEV_GPU_UTIL[7d])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[7d])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[1d])",
		"quantile_over_time(0.95, DCGM_FI_DEV_GPU_UTIL[7d])",
		"max_over_time(timestamp(DCGM_FI_DEV_GPU_UTIL > 5)[7d:5m])",
		"max_over_time(timestamp(DCGM_FI_DEV_GPU_UTIL_RATIO > 0.05)[7d:5m])",
		"max_over_time(DCGM_FI_DEV_FB_USED[7d])",
		"max_over_time(DCGM_FI_DEV_FB_USED_BYTES[7d])",
		"count_over_time(DCGM_FI_DEV_FB_USED[7d:5m])",
		"max_over_time(DCGM_FI_DEV_FB_CAPACITY_BYTES[7d])",
	} {
		if !strings.Contains(queries, want) {
			t.Errorf("missing query %s", want)
		}
	}
	if strings.Contains(queries, "DCGM_FI_PROF") {
		t.Error("profiling fallback queried although every device has utilization")
	}
	if stats.Queries != len(srv.Queries()) {
		t.Errorf("stats.Queries = %d, server saw %d", stats.Queries, len(srv.Queries()))
	}
}

// dcgm-exporter 4.9+: ratios and bytes, lower-case hostname, explicit
// capacity metric. Scraped without honorLabels, so the workload is in the
// exported_* labels and pod/namespace describe the exporter itself.
func TestCollectRenamedMetricsAndExportedLabels(t *testing.T) {
	gpu := labels("gpu", "1", "UUID", "GPU-bbb", "modelName", "NVIDIA H100 80GB HBM3", "hostname", "node-2",
		"pod", "nvidia-dcgm-exporter-x2x", "namespace", "gpu-operator", "container", "nvidia-dcgm-exporter",
		"exported_pod", "train-0", "exported_namespace", "research", "exported_container", "trainer")
	// A GPU nobody uses still carries the exporter's own target labels.
	unused := labels("gpu", "2", "UUID", "GPU-ccc", "modelName", "NVIDIA H100 80GB HBM3",
		"pod", "nvidia-dcgm-exporter-x2x", "namespace", "gpu-operator", "container", "nvidia-dcgm-exporter")
	usage, _, _ := collect(t, []promtest.Series{
		{Metric: "DCGM_FI_DEV_GPU_UTIL_RATIO", Labels: gpu, Avg: 0.88, Max: 1, P95: 0.99, RecentMax: 1, LastActive: now},
		{Metric: "DCGM_FI_DEV_FB_USED_BYTES", Labels: gpu, Avg: 60000 * mib, Max: 72704 * mib, Samples: 100},
		{Metric: "DCGM_FI_DEV_FB_FREE_BYTES", Labels: gpu, Avg: 20000 * mib},
		{Metric: "DCGM_FI_DEV_FB_CAPACITY_BYTES", Labels: gpu, Max: 81559 * mib},
		{Metric: "DCGM_FI_DEV_GPU_UTIL_RATIO", Labels: unused, Avg: 0},
		{Metric: "DCGM_FI_DEV_FB_USED_BYTES", Labels: unused, Avg: 0, Samples: 100},
	}, nil)

	if len(usage) != 2 {
		t.Fatalf("got %d devices: %+v", len(usage), usage)
	}
	// Sorted by namespace: the exporter pod's own entry comes first. It is
	// harmless because the exporter pod requests no GPU and is never joined.
	if usage[0].Namespace != "gpu-operator" {
		t.Errorf("first entry = %+v", usage[0])
	}
	u := usage[1]
	if u.Namespace != "research" || u.Pod != "train-0" || u.Container != "trainer" || u.Host != "node-2" {
		t.Errorf("identity = %+v", u)
	}
	if !near(u.UtilAvg, 88) || !near(u.UtilMax, 100) || !near(u.UtilP95, 99) || !near(u.UtilRecentMax, 100) {
		t.Errorf("ratios not converted to percent: %+v", u)
	}
	if !near(u.MemPeakMiB, 72704) || !near(u.MemAvgMiB, 60000) || !near(u.CapacityMiB, 81559) {
		t.Errorf("bytes not converted to MiB: %+v", u)
	}
	if !u.LastActive.Equal(now) {
		t.Errorf("LastActive = %v (timestamps must not be scaled)", u.LastActive)
	}
	if u.Observed != 100*5*time.Minute {
		t.Errorf("Observed = %v (counts must not be scaled)", u.Observed)
	}
}

func TestCollectMIGFallsBackToProfilingMetrics(t *testing.T) {
	mig := labels("gpu", "0", "UUID", "GPU-ddd", "GPU_I_ID", "3", "GPU_I_PROFILE", "1g.10gb", "modelName", "NVIDIA A100-SXM4-80GB",
		"pod", "tagger-1", "namespace", "ml", "container", "c")
	whole := labels("gpu", "1", "UUID", "GPU-eee", "modelName", "NVIDIA A100-SXM4-80GB", "pod", "big-1", "namespace", "ml", "container", "c")
	usage, _, srv := collect(t, []promtest.Series{
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: mig, Avg: 3000, Max: 4000, Samples: 500},
		{Metric: "DCGM_FI_PROF_GR_ENGINE_ACTIVE", Labels: mig, Avg: 0.3, Max: 0.9, P95: 0.6, RecentMax: 0.9, LastActive: now},
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: whole, Avg: 50, Max: 90, P95: 80, RecentMax: 90, LastActive: now},
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: whole, Avg: 3000, Max: 4000, Samples: 500},
		// Must not replace the device-level utilization of the whole GPU.
		{Metric: "DCGM_FI_PROF_GR_ENGINE_ACTIVE", Labels: whole, Avg: 0.01, Max: 0.01, P95: 0.01, RecentMax: 0.01},
	}, nil)
	if len(usage) != 2 {
		t.Fatalf("got %+v", usage)
	}
	whl, m := usage[0], usage[1]
	if m.Device != "GPU-ddd/mig3" || m.MIGProfile != "1g.10gb" {
		t.Errorf("MIG identity = %+v", m)
	}
	if !m.HasUtil || !near(m.UtilAvg, 30) || !near(m.UtilP95, 60) || !near(m.UtilMax, 90) {
		t.Errorf("MIG utilization from fallback = %+v", m)
	}
	if !near(whl.UtilAvg, 50) || !near(whl.UtilP95, 80) {
		t.Errorf("fallback overwrote primary utilization: %+v", whl)
	}
	if !strings.Contains(strings.Join(srv.Queries(), "\n"), "avg_over_time(DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO[7d])") {
		t.Error("renamed profiling metric not queried")
	}
}

func TestCollectWithoutAnyUtilization(t *testing.T) {
	mig := labels("UUID", "GPU-ddd", "GPU_I_ID", "3", "GPU_I_PROFILE", "1g.10gb", "pod", "p", "namespace", "ml")
	usage, _, _ := collect(t, []promtest.Series{
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: mig, Avg: 3000, Max: 4000, Samples: 500},
	}, nil)
	if len(usage) != 1 || usage[0].HasUtil || usage[0].HasRecent || !usage[0].LastActive.IsZero() || !usage[0].HasMemory {
		t.Errorf("got %+v", usage)
	}
	if usage[0].CapacityMiB != 0 {
		t.Errorf("capacity should be unknown, got %v", usage[0].CapacityMiB)
	}
}

func TestCollectNamespaceFilterAndOptions(t *testing.T) {
	a := labels("UUID", "GPU-1", "pod_name", "old-style", "pod_namespace", "team-a", "container_name", "c")
	b := labels("gpu", "0", "Hostname", "node-9", "pod", "p", "namespace", "team-b")
	series := []promtest.Series{
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: a, Avg: 1, Max: 2, P95: 2, RecentMax: 1},
		{Metric: "DCGM_FI_DEV_FB_USED", Labels: a, Avg: 10, Max: 20, Samples: 99999},
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: b, Avg: 1, Max: 2, P95: 2, RecentMax: 1},
	}
	usage, _, srv := collect(t, series, func(o *dcgm.Options) {
		o.Namespace = "team-a"
		o.Window = 36 * time.Hour
		o.Recent = 90 * time.Minute
		o.Step = 90 * time.Second
		o.IdleUtil = 2.5
	})
	_ = srv
	if len(usage) != 1 || usage[0].Pod != "old-style" || usage[0].Container != "c" {
		t.Fatalf("got %+v", usage)
	}
	if usage[0].Observed != 36*time.Hour {
		t.Errorf("Observed must be capped at the window, got %v", usage[0].Observed)
	}
	if !usage[0].LastActive.IsZero() {
		t.Errorf("LastActive = %v, want zero", usage[0].LastActive)
	}
	queries := strings.Join(srv.Queries(), "\n")
	for _, want := range []string{
		"avg_over_time(DCGM_FI_DEV_GPU_UTIL[36h])",
		"max_over_time(DCGM_FI_DEV_GPU_UTIL[90m])",
		"max_over_time(timestamp(DCGM_FI_DEV_GPU_UTIL > 2.5)[36h:90s])",
		"max_over_time(timestamp(DCGM_FI_DEV_GPU_UTIL_RATIO > 0.025)[36h:90s])",
	} {
		if !strings.Contains(queries, want) {
			t.Errorf("missing query %s in:\n%s", want, queries)
		}
	}

	all, _, _ := collect(t, series, nil)
	if len(all) != 2 || all[1].Device != "node-9/gpu0" {
		t.Errorf("device fallback id: %+v", all)
	}
}

func TestCollectErrors(t *testing.T) {
	srv := promtest.New("7d", nil)
	defer srv.Close()
	srv.FailQueries(regexp.MustCompile(`quantile_over_time`))
	tr, _ := prom.NewHTTPTransport(srv.URL, "", 10*time.Second)
	_, _, err := dcgm.Collect(context.Background(), prom.NewClient(tr), dcgm.Options{Window: 7 * 24 * time.Hour, Now: now})
	if err == nil || !strings.Contains(err.Error(), "query failed on purpose") || !strings.Contains(err.Error(), "quantile_over_time") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := dcgm.Collect(context.Background(), prom.NewClient(tr), dcgm.Options{}); err == nil {
		t.Error("zero window should fail")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.FailQueries(nil)
	if _, _, err := dcgm.Collect(ctx, prom.NewClient(tr), dcgm.Options{Window: time.Hour, Now: now}); err == nil {
		t.Error("cancelled context should fail")
	}
}

func TestEmptyPrometheus(t *testing.T) {
	usage, stats, _ := collect(t, nil, nil)
	if len(usage) != 0 || stats.Series != 0 || len(stats.Metrics) != 0 || stats.Queries == 0 {
		t.Errorf("usage=%v stats=%+v", usage, stats)
	}
}

func TestPodRefFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   dcgm.PodRef
		ok     bool
	}{
		{"native", labels("pod", "p", "namespace", "n", "container", "c"), dcgm.PodRef{Namespace: "n", Pod: "p", Container: "c"}, true},
		{"exported wins", labels("pod", "exp", "namespace", "mon", "exported_pod", "p", "exported_namespace", "n", "exported_container", "c"), dcgm.PodRef{Namespace: "n", Pod: "p", Container: "c"}, true},
		{"exported without namespace is not trusted", labels("pod", "exp", "namespace", "mon", "exported_pod", "p"), dcgm.PodRef{}, false},
		{"old names", labels("pod_name", "p", "pod_namespace", "n", "container_name", "c"), dcgm.PodRef{Namespace: "n", Pod: "p", Container: "c"}, true},
		{"pod without namespace", labels("pod", "p"), dcgm.PodRef{}, false},
		{"bare gpu", labels("gpu", "0", "UUID", "GPU-x"), dcgm.PodRef{}, false},
	}
	for _, tt := range tests {
		got, ok := dcgm.PodRefFromLabels(tt.labels)
		if ok != tt.ok || got != tt.want {
			t.Errorf("%s: got %+v, %v", tt.name, got, ok)
		}
	}
}

func TestMetricNames(t *testing.T) {
	names := strings.Join(dcgm.MetricNames(), ",")
	for _, want := range []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_GPU_UTIL_RATIO", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_FB_USED_BYTES", "DCGM_FI_DEV_FB_FREE"} {
		if !strings.Contains(names, want) {
			t.Errorf("missing %s", want)
		}
	}
}
