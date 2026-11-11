package report

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/dcgm"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kube"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/pricing"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/recommend"
)

var now = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

const week = 7 * 24 * time.Hour

func wl(ns, kind, name, resource string, count int, pods ...string) kube.Workload {
	w := kube.Workload{Namespace: ns, Kind: kind, Name: name}
	for _, p := range pods {
		w.Pods = append(w.Pods, kube.Pod{Namespace: ns, Name: p, Node: "node-1", Request: kube.Request{Resource: resource, Count: count}})
	}
	return w
}

// use returns a week of usage for one whole GPU.
func use(ns, pod, model string, peakMiB, avg, p95 float64) dcgm.Usage {
	return dcgm.Usage{
		Namespace: ns, Pod: pod, Container: "main", Device: "GPU-" + pod, ModelName: model,
		HasUtil: true, UtilAvg: avg, UtilP95: p95, UtilMax: p95, HasRecent: true, UtilRecentMax: p95, LastActive: now,
		HasMemory: true, MemPeakMiB: peakMiB, MemAvgMiB: peakMiB / 2, Observed: week,
	}
}

func build(t *testing.T, ws []kube.Workload, usage []dcgm.Usage, nodes map[string]kube.NodeInfo) []Row {
	t.Helper()
	return Build(Input{Workloads: ws, Usage: usage, Nodes: nodes, Prices: pricing.Defaults(), Options: recommend.DefaultOptions(), Now: now})
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

func TestBuildWholeGPUWorkload(t *testing.T) {
	ws := []kube.Workload{wl("ml", "Deployment", "embed-api", kube.ResourceGPU, 1, "e-1", "e-2", "e-3", "e-4")}
	usage := []dcgm.Usage{
		use("ml", "e-1", "NVIDIA A100-SXM4-80GB", 6246, 8, 10),
		use("ml", "e-2", "NVIDIA A100-SXM4-80GB", 5000, 10, 12),
		use("ml", "e-3", "NVIDIA A100-SXM4-80GB", 4000, 12, 11),
		use("ml", "e-4", "NVIDIA A100-SXM4-80GB", 3000, 14, 9),
		use("ml", "someone-else", "Tesla T4", 15000, 99, 99),
	}
	rows := build(t, ws, usage, nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	if r.Replicas != 4 || r.GPUs != 4 || r.PodsWithMetrics != 4 || len(r.Pods) != 4 {
		t.Errorf("counts: %+v", r)
	}
	if r.Requested.Text != "1x A100" || r.Requested.Model != "a100-80gb" || r.Requested.ModelName != "A100 80GB" || r.Requested.Sharing != "" {
		t.Errorf("requested = %+v", r.Requested)
	}
	// Peak is the worst replica, average the mean, p95 the worst replica.
	if *r.PeakMemMiB != 6246 || *r.AvgUtil != 11 || *r.P95Util != 12 {
		t.Errorf("aggregates: peak %v avg %v p95 %v", *r.PeakMemMiB, *r.AvgUtil, *r.P95Util)
	}
	if r.Recommendation.Action != recommend.ActionMIG || r.Recommendation.Target != "1g.10gb" {
		t.Errorf("recommendation = %+v", r.Recommendation)
	}
	wantCost := 4 * 5.07 * pricing.HoursPerMonth
	if !approx(r.MonthlyCost, wantCost) || !approx(r.MonthlyWaste, wantCost*6/7) || *r.HourlyPrice != 5.07 {
		t.Errorf("cost %v waste %v, want %v / %v", r.MonthlyCost, r.MonthlyWaste, wantCost, wantCost*6/7)
	}
	if r.ObservedSeconds != week.Seconds() || r.IdleSeconds != 0 {
		t.Errorf("observed %v idle %v", r.ObservedSeconds, r.IdleSeconds)
	}
}

func TestBuildIdleNeedsEveryReplicaIdle(t *testing.T) {
	idle := func(pod string, last time.Duration) dcgm.Usage {
		u := use("nb", pod, "NVIDIA A100-SXM4-80GB", 300, 0, 0)
		u.UtilRecentMax = 0
		u.LastActive = now.Add(-last)
		return u
	}
	ws := []kube.Workload{wl("nb", "StatefulSet", "notebooks", kube.ResourceGPU, 1, "n-0", "n-1")}

	rows := build(t, ws, []dcgm.Usage{idle("n-0", 5*24*time.Hour), idle("n-1", 30*time.Hour)}, nil)
	r := rows[0]
	if r.Recommendation.Action != recommend.ActionReclaim || r.Recommendation.Summary != "idle 30h, reclaim" {
		t.Errorf("idle time must be that of the most recently active replica: %+v", r.Recommendation)
	}
	if r.IdleSeconds != (30*time.Hour).Seconds() || !approx(r.MonthlyWaste, r.MonthlyCost) || r.MonthlyCost == 0 {
		t.Errorf("idle %v cost %v waste %v", r.IdleSeconds, r.MonthlyCost, r.MonthlyWaste)
	}

	// One busy replica keeps the workload from being reclaimed.
	busy := use("nb", "n-1", "NVIDIA A100-SXM4-80GB", 70000, 60, 95)
	rows = build(t, ws, []dcgm.Usage{idle("n-0", 5*24*time.Hour), busy}, nil)
	if rows[0].Recommendation.Action != recommend.ActionKeep {
		t.Errorf("got %+v", rows[0].Recommendation)
	}

	// A replica without metrics means idleness cannot be established.
	rows = build(t, ws, []dcgm.Usage{idle("n-0", 5*24*time.Hour)}, nil)
	r = rows[0]
	if r.Recommendation.Action == recommend.ActionReclaim {
		t.Errorf("reclaim recommended although a replica has no metrics: %+v", r.Recommendation)
	}
	if r.PodsWithMetrics != 1 || !strings.Contains(r.Recommendation.Reason, "only 1 of 2 replicas have metrics") {
		t.Errorf("partial-metrics note missing: %q", r.Recommendation.Reason)
	}

	// Never active inside the window: idle time is a lower bound.
	never := idle("n-0", 0)
	never.LastActive = time.Time{}
	rows = build(t, []kube.Workload{wl("nb", "Pod", "n-0", kube.ResourceGPU, 1, "n-0")}, []dcgm.Usage{never}, nil)
	if rows[0].Recommendation.Summary != "idle 7+ days, reclaim" {
		t.Errorf("summary = %q", rows[0].Recommendation.Summary)
	}
}

func TestBuildNoMetricsFallsBackToNodeLabels(t *testing.T) {
	nodes := map[string]kube.NodeInfo{"node-1": {Name: "node-1", Product: "NVIDIA-H100-80GB-HBM3", MemoryMiB: 81559}}
	rows := build(t, []kube.Workload{wl("x", "Pod", "p", kube.ResourceGPU, 2, "p")}, nil, nodes)
	r := rows[0]
	if r.Requested.Text != "2x H100" || r.Requested.Model != "h100" {
		t.Errorf("requested = %+v", r.Requested)
	}
	if r.Recommendation.Action != recommend.ActionUnknown || r.Recommendation.Summary != "no data" {
		t.Errorf("recommendation = %+v", r.Recommendation)
	}
	if r.PeakMemMiB != nil || r.AvgUtil != nil || r.MonthlyWaste != 0 || *r.CapacityMiB != 81559 {
		t.Errorf("row = %+v", r)
	}
	if !approx(r.MonthlyCost, 2*9.80*pricing.HoursPerMonth) {
		t.Errorf("cost = %v", r.MonthlyCost)
	}

	// No model anywhere.
	rows = build(t, []kube.Workload{wl("x", "Pod", "p", kube.ResourceGPU, 1, "p")}, nil, nil)
	r = rows[0]
	if r.Requested.Text != "1x GPU" || r.HourlyPrice != nil || r.MonthlyCost != 0 || r.CapacityMiB != nil {
		t.Errorf("row = %+v", r)
	}
}

func TestBuildMIGWorkload(t *testing.T) {
	u := use("ml", "t-1", "NVIDIA A100-SXM4-80GB", 3000, 0, 0)
	u.HasUtil, u.HasRecent, u.MIGProfile, u.CapacityMiB = false, false, "1g.10gb", 9856
	u.LastActive = time.Time{}

	// Mixed strategy: the profile is in the resource name.
	rows := build(t, []kube.Workload{wl("ml", "Deployment", "tagger", "nvidia.com/mig-1g.10gb", 1, "t-1")}, []dcgm.Usage{u}, nil)
	r := rows[0]
	if r.Requested.Text != "1x A100 MIG 1g.10gb" || r.Requested.Sharing != "mig" || r.Requested.MIGProfile != "1g.10gb" || r.Requested.Model != "a100-80gb" {
		t.Errorf("requested = %+v", r.Requested)
	}
	if !approx(r.GPUs, 1.0/7) || !approx(r.MonthlyCost, 5.07*pricing.HoursPerMonth/7) {
		t.Errorf("gpus %v cost %v", r.GPUs, r.MonthlyCost)
	}
	if r.Recommendation.Action != recommend.ActionKeep || r.MonthlyWaste != 0 {
		t.Errorf("recommendation = %+v", r.Recommendation)
	}

	// Single strategy: plain nvidia.com/gpu, profile known from DCGM.
	rows = build(t, []kube.Workload{wl("ml", "Deployment", "tagger", kube.ResourceGPU, 1, "t-1")}, []dcgm.Usage{u}, nil)
	if rows[0].Requested.Sharing != "mig" || rows[0].Requested.MIGProfile != "1g.10gb" {
		t.Errorf("requested = %+v", rows[0].Requested)
	}

	// Single strategy without metrics: profile from the GFD product label.
	nodes := map[string]kube.NodeInfo{"node-1": {Name: "node-1", Product: "NVIDIA-A100-SXM4-40GB-MIG-1g.5gb", MemoryMiB: 4864}}
	rows = build(t, []kube.Workload{wl("ml", "Deployment", "tagger", kube.ResourceGPU, 1, "t-1")}, nil, nodes)
	r = rows[0]
	if r.Requested.Text != "1x A100 MIG 1g.5gb" || r.Requested.Model != "a100-40gb" || !approx(r.GPUs, 1.0/7) {
		t.Errorf("requested = %+v gpus=%v", r.Requested, r.GPUs)
	}
}

func TestBuildSharedGPU(t *testing.T) {
	idle := use("ml", "s-1", "Tesla T4", 500, 0, 0)
	idle.UtilRecentMax, idle.LastActive = 0, now.Add(-72*time.Hour)
	ws := []kube.Workload{wl("ml", "Deployment", "small", kube.ResourceGPU, 1, "s-1")}

	nodes := map[string]kube.NodeInfo{"node-1": {Name: "node-1", Product: "Tesla-T4-SHARED", SharingStrategy: "time-slicing", Replicas: 4}}
	r := build(t, ws, []dcgm.Usage{idle}, nodes)[0]
	if r.Requested.Text != "1x T4 (shared)" || r.Requested.Sharing != "time-slicing" || r.GPUs != 0.25 {
		t.Errorf("requested = %+v gpus=%v", r.Requested, r.GPUs)
	}
	if r.Recommendation.Action != recommend.ActionReclaim || !approx(r.MonthlyWaste, 0.53*pricing.HoursPerMonth/4) {
		t.Errorf("recommendation %+v waste %v", r.Recommendation, r.MonthlyWaste)
	}

	// Renamed resource, replica count unknown: savings cannot be priced.
	ws = []kube.Workload{wl("ml", "Deployment", "small", kube.ResourceGPUShared, 1, "s-1")}
	r = build(t, ws, []dcgm.Usage{idle}, nil)[0]
	if r.Requested.Sharing != "time-slicing" || r.MonthlyCost != 0 || r.MonthlyWaste != 0 {
		t.Errorf("row = %+v", r)
	}
	if fmtWaste(r) != "?" || !strings.Contains(r.Recommendation.Reason, "share of the physical GPU") {
		t.Errorf("waste %q reason %q", fmtWaste(r), r.Recommendation.Reason)
	}

	// MPS is recognised too.
	nodes["node-1"] = kube.NodeInfo{Name: "node-1", Product: "Tesla-T4", SharingStrategy: "mps", Replicas: 2}
	ws = []kube.Workload{wl("ml", "Deployment", "small", kube.ResourceGPU, 1, "s-1")}
	if r = build(t, ws, []dcgm.Usage{idle}, nodes)[0]; r.Requested.Sharing != "mps" || r.GPUs != 0.5 {
		t.Errorf("row = %+v", r)
	}
}

func TestBuildUnknownModelHasNoPrice(t *testing.T) {
	u := use("x", "p", "GeForce RTX 4090", 2000, 3, 5)
	u.CapacityMiB = 24564
	r := build(t, []kube.Workload{wl("x", "Pod", "p", kube.ResourceGPU, 1, "p")}, []dcgm.Usage{u}, nil)[0]
	if r.Requested.Text != "1x GPU" || r.Requested.ModelName != "GeForce RTX 4090" || r.Requested.Model != "" {
		t.Errorf("requested = %+v", r.Requested)
	}
	if r.Recommendation.Action != recommend.ActionTimeSlice {
		t.Errorf("recommendation = %+v", r.Recommendation)
	}
	if r.MonthlyWaste != 0 || fmtWaste(r) != "?" || !strings.Contains(r.Recommendation.Reason, "No price is known") {
		t.Errorf("waste %v %q reason %q", r.MonthlyWaste, fmtWaste(r), r.Recommendation.Reason)
	}
}

func TestBuildUsesTheLargestRequestAndAmbiguousModel(t *testing.T) {
	w := wl("x", "Job", "j", kube.ResourceGPU, 1, "a", "b")
	w.Pods[1].Request.Count = 2
	ua := use("x", "a", "A100", 1000, 90, 99)
	ua.CapacityMiB = 81920 // capacity settles the 40/80 GB question
	r := build(t, []kube.Workload{w, {Namespace: "x", Kind: "Job", Name: "empty"}}, []dcgm.Usage{ua}, nil)
	if len(r) != 1 {
		t.Fatalf("workloads without pods must be skipped: %+v", r)
	}
	if r[0].Requested.Count != 2 || r[0].Requested.Model != "a100-80gb" || r[0].GPUs != 4 {
		t.Errorf("row = %+v", r[0])
	}
}

func rowsForSorting() []Row {
	f := func(v float64) *float64 { return &v }
	return []Row{
		{Namespace: "b", Workload: "beta", MonthlyWaste: 100, AvgUtil: f(50), PeakMemMiB: f(10), GPUs: 1},
		{Namespace: "a", Workload: "delta", MonthlyWaste: 0, AvgUtil: nil, PeakMemMiB: nil, GPUs: 8},
		{Namespace: "a", Workload: "alpha", MonthlyWaste: 900, AvgUtil: f(5), PeakMemMiB: f(70), GPUs: 2},
		{Namespace: "c", Workload: "gamma", MonthlyWaste: 100, AvgUtil: f(20), PeakMemMiB: f(30), GPUs: 2},
	}
}

func TestSort(t *testing.T) {
	want := map[string]string{
		"waste":     "alpha beta gamma delta",
		"name":      "alpha beta delta gamma",
		"namespace": "alpha delta beta gamma",
		"util":      "alpha gamma beta delta",
		"mem":       "alpha gamma beta delta",
		"gpus":      "delta alpha gamma beta",
	}
	if len(want) != len(SortKeys) {
		t.Fatalf("test covers %d keys, SortKeys has %d", len(want), len(SortKeys))
	}
	for key, order := range want {
		rows := rowsForSorting()
		if err := Sort(rows, key); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.Workload)
		}
		if strings.Join(got, " ") != order {
			t.Errorf("sort by %s: got %v, want %s", key, got, order)
		}
	}
	if err := Sort(nil, "color"); err == nil {
		t.Error("unknown key should fail")
	}
}

func TestSummarizeAndFilter(t *testing.T) {
	rows := rowsForSorting()
	for i := range rows {
		rows[i].MonthlyCost = 1000
	}
	s := Summarize(rows)
	if s.Workloads != 4 || s.Shown != 4 || s.GPUs != 13 || s.MonthlyWaste != 1100 || s.MonthlyCost != 4000 {
		t.Errorf("summary = %+v", s)
	}
	if got := FilterMinWaste(rows, 100); len(got) != 3 {
		t.Errorf("filter 100: %d rows", len(got))
	}
	if got := FilterMinWaste(rows, 101); len(got) != 1 || got[0].Workload != "alpha" {
		t.Errorf("filter 101: %+v", got)
	}
	if got := FilterMinWaste(rows, 0); len(got) != 4 {
		t.Errorf("filter 0: %d rows", len(got))
	}
}

func TestFormatMoney(t *testing.T) {
	for v, want := range map[float64]string{
		0: "$0", 4: "$0", 5: "$10", 383.2: "$380", 995: "$1,000", 1049: "$1,000", 2679: "$2,700",
		9186: "$9,200", 12689: "$13,000", 123456: "$120,000", 1.5e6: "$1,500,000", -3: "$0",
	} {
		if got := FormatMoney(v); got != want {
			t.Errorf("FormatMoney(%v) = %s, want %s", v, got, want)
		}
	}
}
