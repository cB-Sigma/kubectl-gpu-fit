// Package report joins Kubernetes workloads with DCGM usage, runs the
// recommendation engine and renders the result.
package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/dcgm"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kube"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/pricing"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/recommend"
)

// PriceNote is attached to every report so the figures are never mistaken
// for a bill.
const PriceNote = "Costs are estimates from rough on-demand list prices per GPU-hour; override with --price or --price-file."

// CustomPriceNote replaces PriceNote when the user supplied prices.
const CustomPriceNote = "Costs are estimates: your prices where given, rough built-in on-demand list prices for other models."

// Requested describes what each replica of a workload asks for.
type Requested struct {
	// Count is the number of devices per replica.
	Count int `json:"count"`
	// Resource is the extended resource name, e.g. nvidia.com/gpu.
	Resource string `json:"resource"`
	// Model is the catalog key of the GPU model, empty if unknown.
	Model string `json:"model,omitempty"`
	// ModelName is a human-readable model name.
	ModelName string `json:"modelName,omitempty"`
	// Sharing is "", "mig", "time-slicing" or "mps".
	Sharing string `json:"sharing,omitempty"`
	// MIGProfile is the MIG profile in use, if any.
	MIGProfile string `json:"migProfile,omitempty"`
	// Text is the compact form shown in the table, e.g. "1x A100".
	Text string `json:"text"`
}

// Row is the report line for one workload.
type Row struct {
	Namespace string    `json:"namespace"`
	Workload  string    `json:"workload"`
	Kind      string    `json:"kind"`
	Replicas  int       `json:"replicas"`
	Requested Requested `json:"requested"`
	// GPUs is the workload's total footprint in whole-GPU equivalents.
	GPUs float64 `json:"gpus"`

	// Usage figures are nil when the metric is missing.
	PeakMemMiB  *float64 `json:"peakMemMiB"`
	CapacityMiB *float64 `json:"capacityMiB"`
	AvgUtil     *float64 `json:"avgUtilPercent"`
	P95Util     *float64 `json:"p95UtilPercent"`
	MaxUtil     *float64 `json:"maxUtilPercent"`
	// ObservedSeconds is how much of the window has metrics.
	ObservedSeconds float64 `json:"observedSeconds"`
	// IdleSeconds is how long the workload has been idle, if it is.
	IdleSeconds float64 `json:"idleSeconds,omitempty"`
	// PodsWithMetrics counts replicas that have DCGM data.
	PodsWithMetrics int `json:"podsWithMetrics"`

	Recommendation recommend.Result `json:"recommendation"`

	// HourlyPrice is the price of one whole GPU; nil if unknown.
	HourlyPrice  *float64 `json:"hourlyPriceUSD"`
	MonthlyCost  float64  `json:"monthlyCostUSD"`
	MonthlyWaste float64  `json:"monthlyWasteUSD"`

	Pods []string `json:"pods"`
}

// Summary totals the report.
type Summary struct {
	Workloads    int     `json:"workloads"`
	GPUs         float64 `json:"gpus"`
	MonthlyCost  float64 `json:"monthlyCostUSD"`
	MonthlyWaste float64 `json:"monthlyWasteUSD"`
	// Shown is the number of rows left after filtering.
	Shown int `json:"shown"`
}

// Report is the full result.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Window is the look-back period, e.g. "7d".
	Window string `json:"window"`
	// Headroom is the memory safety margin (0.2 = 20%).
	Headroom float64 `json:"headroom"`
	// Namespace is empty when all namespaces were analysed.
	Namespace string `json:"namespace,omitempty"`
	// Source describes where the metrics came from.
	Source    string   `json:"source"`
	Rows      []Row    `json:"rows"`
	Summary   Summary  `json:"summary"`
	PriceNote string   `json:"priceNote"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Input is everything Build needs.
type Input struct {
	Workloads []kube.Workload
	Nodes     map[string]kube.NodeInfo
	Usage     []dcgm.Usage
	Prices    *pricing.Table
	Options   recommend.Options
	Now       time.Time
}

// Build produces one row per workload, sorted by namespace and name.
func Build(in Input) []Row {
	byPod := map[string][]dcgm.Usage{}
	for _, u := range in.Usage {
		key := u.Namespace + "/" + u.Pod
		byPod[key] = append(byPod[key], u)
	}
	rows := make([]Row, 0, len(in.Workloads))
	for _, w := range in.Workloads {
		if len(w.Pods) == 0 {
			continue
		}
		rows = append(rows, buildRow(w, in, byPod))
	}
	return rows
}

func buildRow(w kube.Workload, in Input, byPod map[string][]dcgm.Usage) Row {
	row := Row{Namespace: w.Namespace, Workload: w.Name, Kind: w.Kind, Replicas: len(w.Pods)}

	req := w.Pods[0].Request
	var node kube.NodeInfo
	for _, p := range w.Pods {
		row.Pods = append(row.Pods, p.Name)
		if p.Request.Count > req.Count {
			req = p.Request
		}
		if node.Name == "" {
			node = in.Nodes[p.Node]
		}
	}

	var usages []dcgm.Usage
	allPodsRecent := true
	for _, p := range w.Pods {
		us := byPod[p.Namespace+"/"+p.Name]
		if len(us) > 0 {
			row.PodsWithMetrics++
		}
		recent := false
		for _, u := range us {
			recent = recent || u.HasRecent
		}
		allPodsRecent = allPodsRecent && recent
		usages = append(usages, us...)
	}

	// Sharing mode and MIG profile.
	sharing := recommend.SharingNone
	profile := gpu.NormalizeProfileName(req.MIGProfile())
	if profile == "" {
		profile = gpu.NormalizeProfileName(mostCommon(usages, func(u dcgm.Usage) string { return u.MIGProfile }))
	}
	if profile == "" {
		profile = gpu.MIGProfileFromProduct(node.Product)
	}
	sharedReplicas := 0
	switch {
	case profile != "":
		sharing = recommend.SharingMIG
	case node.SharingStrategy == "mps":
		sharing, sharedReplicas = recommend.SharingMPS, node.Replicas
	case node.SharingStrategy == "time-slicing" || req.Shared():
		sharing, sharedReplicas = recommend.SharingTimeSlice, node.Replicas
	}

	// Aggregate usage across every device of every replica.
	agg := aggregate(usages, in.Now)
	agg.hasRecent = agg.hasRecent && allPodsRecent

	// GPU model: DCGM's model string first, GFD's node label second.
	var model *gpu.Model
	capacityHint := agg.capacity
	if sharing == recommend.SharingMIG {
		capacityHint = 0 // a slice's size says nothing about the card
	}
	modelName := mostCommon(usages, func(u dcgm.Usage) string { return u.ModelName })
	if m, ok := gpu.Resolve(modelName, capacityHint); ok {
		model = m
	} else {
		if capacityHint == 0 && sharing != recommend.SharingMIG {
			capacityHint = node.MemoryMiB
		}
		if m, ok := gpu.Resolve(node.Product, capacityHint); ok {
			model = m
		}
		if modelName == "" {
			modelName = strings.ReplaceAll(node.Product, "-", " ")
		}
	}

	// Device capacity as the pod sees it.
	capacity := agg.capacity
	if capacity == 0 {
		switch {
		case sharing == recommend.SharingMIG && model != nil:
			if p, ok := model.Profile(profile); ok {
				capacity = p.MemoryMiB()
			}
		case sharing == recommend.SharingMIG:
			// unknown card: leave capacity unknown
		case node.MemoryMiB > 0:
			capacity = node.MemoryMiB
		case model != nil:
			capacity = model.MemoryMiB()
		}
	}

	// Share of one physical GPU that each requested device stands for.
	share, shareKnown := 1.0, true
	switch sharing {
	case recommend.SharingMIG:
		shareKnown = false
		if model != nil {
			if p, ok := model.Profile(profile); ok {
				share, shareKnown = p.Share(), true
			}
		}
	case recommend.SharingTimeSlice, recommend.SharingMPS:
		if sharedReplicas > 1 {
			share = 1 / float64(sharedReplicas)
		} else {
			shareKnown = false
		}
	}

	row.Requested = Requested{
		Count:      req.Count,
		Resource:   req.Resource,
		Sharing:    string(sharing),
		MIGProfile: profile,
		ModelName:  modelName,
	}
	if model != nil {
		row.Requested.Model = model.Key
		row.Requested.ModelName = model.Display
	}
	row.Requested.Text = requestedText(req.Count, model, sharing, profile)

	devices := float64(req.Count * len(w.Pods))
	row.GPUs = devices * share

	price, priceKnown := in.Prices.Lookup(model)
	engineIn := recommend.Input{
		Model:         model,
		CapacityMiB:   capacity,
		GPUsPerPod:    req.Count,
		Sharing:       sharing,
		MIGProfile:    profile,
		HasMemory:     agg.hasMem,
		HasUtil:       agg.hasUtil,
		PeakMemMiB:    agg.peakMem,
		AvgUtil:       agg.avgUtil,
		P95Util:       agg.p95Util,
		MaxUtil:       agg.maxUtil,
		RecentMaxUtil: agg.recentMax,
		HasRecent:     agg.hasRecent,
		Observed:      agg.observed,
		IdleFor:       agg.idleFor,
		NeverActive:   agg.neverActive,
		PriceOf:       in.Prices.Lookup,
	}
	if priceKnown {
		engineIn.HourlyPrice = price
	}
	row.Recommendation = recommend.Recommend(engineIn, in.Options)

	if row.PodsWithMetrics > 0 && row.PodsWithMetrics < len(w.Pods) {
		row.Recommendation.Reason += fmt.Sprintf(" Note: only %d of %d replicas have metrics.", row.PodsWithMetrics, len(w.Pods))
	}

	if agg.hasMem {
		row.PeakMemMiB = ptr(agg.peakMem)
	}
	if capacity > 0 {
		row.CapacityMiB = ptr(capacity)
	}
	if agg.hasUtil {
		row.AvgUtil, row.P95Util, row.MaxUtil = ptr(agg.avgUtil), ptr(agg.p95Util), ptr(agg.maxUtil)
	}
	row.ObservedSeconds = agg.observed.Seconds()
	if row.Recommendation.Action == recommend.ActionReclaim {
		row.IdleSeconds = agg.idleFor.Seconds()
	}

	if priceKnown {
		row.HourlyPrice = ptr(price)
		if shareKnown {
			row.MonthlyCost = price * share * devices * pricing.HoursPerMonth
			row.MonthlyWaste = row.MonthlyCost * (1 - row.Recommendation.KeepFraction)
		}
	}
	if row.Recommendation.KeepFraction < 1 && row.MonthlyWaste == 0 {
		switch {
		case !priceKnown:
			row.Recommendation.Reason += " No price is known for this GPU model, so no waste figure is given."
		case !shareKnown:
			row.Recommendation.Reason += " The share of the physical GPU behind this request is unknown, so no waste figure is given."
		}
	}
	return row
}

type aggregated struct {
	hasMem, hasUtil, hasRecent bool
	peakMem, capacity          float64
	avgUtil, p95Util, maxUtil  float64
	recentMax                  float64
	observed, idleFor          time.Duration
	neverActive                bool
}

func aggregate(usages []dcgm.Usage, now time.Time) aggregated {
	a := aggregated{hasRecent: true, neverActive: true}
	utilDevices := 0
	sumAvg := 0.0
	first := true
	for _, u := range usages {
		if u.Observed > a.observed {
			a.observed = u.Observed
		}
		a.capacity = math.Max(a.capacity, u.CapacityMiB)
		if u.HasMemory {
			a.hasMem = true
			a.peakMem = math.Max(a.peakMem, u.MemPeakMiB)
		}
		if !u.HasUtil {
			continue
		}
		a.hasUtil = true
		utilDevices++
		sumAvg += u.UtilAvg
		a.p95Util = math.Max(a.p95Util, u.UtilP95)
		a.maxUtil = math.Max(a.maxUtil, u.UtilMax)
		if u.HasRecent {
			a.recentMax = math.Max(a.recentMax, u.UtilRecentMax)
		} else {
			a.hasRecent = false
		}
		// The workload has been idle only as long as its most recently
		// active device.
		idle := u.Observed
		if !u.LastActive.IsZero() {
			a.neverActive = false
			idle = now.Sub(u.LastActive)
			if idle < 0 {
				idle = 0
			}
		}
		if first || idle < a.idleFor {
			a.idleFor = idle
		}
		first = false
	}
	if utilDevices == 0 {
		a.hasRecent, a.neverActive = false, false
	} else {
		a.avgUtil = sumAvg / float64(utilDevices)
	}
	return a
}

func requestedText(count int, model *gpu.Model, sharing recommend.Sharing, profile string) string {
	name := "GPU"
	if model != nil {
		name = model.Short
	}
	switch sharing {
	case recommend.SharingMIG:
		return fmt.Sprintf("%dx %s MIG %s", count, name, profile)
	case recommend.SharingTimeSlice, recommend.SharingMPS:
		return fmt.Sprintf("%dx %s (shared)", count, name)
	}
	return fmt.Sprintf("%dx %s", count, name)
}

func mostCommon(usages []dcgm.Usage, field func(dcgm.Usage) string) string {
	counts := map[string]int{}
	best := ""
	for _, u := range usages {
		v := field(u)
		if v == "" {
			continue
		}
		counts[v]++
		if counts[v] > counts[best] || (counts[v] == counts[best] && v < best) {
			best = v
		}
	}
	return best
}

func ptr(f float64) *float64 { return &f }

// SortKeys lists the accepted --sort-by values.
var SortKeys = []string{"waste", "name", "namespace", "util", "mem", "gpus"}

// Sort orders rows in place. Ties always fall back to namespace and name so
// output is deterministic.
func Sort(rows []Row, key string) error {
	byName := func(a, b Row) bool {
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		return a.Kind < b.Kind
	}
	val := func(p *float64, missing float64) float64 {
		if p == nil {
			return missing
		}
		return *p
	}
	var less func(a, b Row) (bool, bool) // (a before b, differ)
	switch key {
	case "waste":
		less = func(a, b Row) (bool, bool) { return a.MonthlyWaste > b.MonthlyWaste, a.MonthlyWaste != b.MonthlyWaste }
	case "namespace":
		less = func(_, _ Row) (bool, bool) { return false, false }
	case "name":
		less = func(a, b Row) (bool, bool) { return a.Workload < b.Workload, a.Workload != b.Workload }
	case "util":
		// Least utilized first; workloads without data go last.
		less = func(a, b Row) (bool, bool) {
			x, y := val(a.AvgUtil, math.Inf(1)), val(b.AvgUtil, math.Inf(1))
			return x < y, x != y
		}
	case "mem":
		less = func(a, b Row) (bool, bool) {
			x, y := val(a.PeakMemMiB, -1), val(b.PeakMemMiB, -1)
			return x > y, x != y
		}
	case "gpus":
		less = func(a, b Row) (bool, bool) { return a.GPUs > b.GPUs, a.GPUs != b.GPUs }
	default:
		return fmt.Errorf("unknown sort key %q (known: %s)", key, strings.Join(SortKeys, ", "))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if before, differ := less(rows[i], rows[j]); differ {
			return before
		}
		return byName(rows[i], rows[j])
	})
	return nil
}

// Summarize totals all rows; call it before filtering.
func Summarize(rows []Row) Summary {
	s := Summary{Workloads: len(rows), Shown: len(rows)}
	for _, r := range rows {
		s.GPUs += r.GPUs
		s.MonthlyCost += r.MonthlyCost
		s.MonthlyWaste += r.MonthlyWaste
	}
	return s
}

// FilterMinWaste keeps rows whose estimated monthly waste is at least min.
func FilterMinWaste(rows []Row, minWaste float64) []Row {
	if minWaste <= 0 {
		return rows
	}
	out := rows[:0:0]
	for _, r := range rows {
		if r.MonthlyWaste >= minWaste {
			out = append(out, r)
		}
	}
	return out
}
