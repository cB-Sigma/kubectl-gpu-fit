// Package recommend contains the right-sizing decision logic. It is pure:
// it takes observed usage for one workload and returns a recommendation,
// without talking to Kubernetes or Prometheus.
package recommend

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
)

// Action is the kind of recommendation.
type Action string

// Actions, from "nothing to do" to "give the GPU back".
const (
	// ActionKeep means the current request looks right, or there is no safe
	// smaller option.
	ActionKeep Action = "keep"
	// ActionUnknown means there is not enough data to say anything.
	ActionUnknown Action = "unknown"
	// ActionReview flags something worth a human look without a concrete,
	// safe target (and therefore without a waste figure).
	ActionReview Action = "review"
	// ActionMIG means the workload fits a MIG slice of its current GPU.
	ActionMIG Action = "mig"
	// ActionTimeSlice means the workload could share its GPU via
	// time-slicing.
	ActionTimeSlice Action = "time-slice"
	// ActionDownsize means a smaller, cheaper GPU model would do.
	ActionDownsize Action = "downsize"
	// ActionReclaim means the GPU is idle and can be given back.
	ActionReclaim Action = "reclaim"
)

// Sharing describes how the workload's GPU is already shared, if at all.
type Sharing string

// Sharing modes.
const (
	SharingNone      Sharing = ""
	SharingMIG       Sharing = "mig"
	SharingTimeSlice Sharing = "time-slicing"
	SharingMPS       Sharing = "mps"
)

// Strategy names accepted in Options.Strategies.
const (
	StrategyMIG       = "mig"
	StrategyDownsize  = "downsize"
	StrategyTimeSlice = "time-slice"
)

// AllStrategies lists every optional strategy in order of preference: the
// first one that yields a safe target wins. MIG comes first because it keeps
// the hardware and gives hard memory and fault isolation; time-slicing keeps
// the hardware but has no isolation; a different GPU model needs new nodes.
var AllStrategies = []string{StrategyMIG, StrategyTimeSlice, StrategyDownsize}

const (
	// computeMargin is the share of a MIG slice's (or time-slice's) compute
	// that the observed p95 utilization may fill. Leaving 10% spare keeps a
	// recommendation from landing exactly on the limit.
	computeMargin = 0.9
	// downsizeMaxUtil is the highest projected p95 utilization (percent)
	// accepted on a smaller GPU model. The cross-model throughput index is
	// coarse, so the projection must leave half the target GPU free.
	downsizeMaxUtil = 50.0
	// reviewP95Util is the p95 utilization (percent) below which a
	// multi-GPU workload is flagged for review.
	reviewP95Util = 50.0
)

// Input is everything the engine knows about one workload.
type Input struct {
	// Model is the resolved GPU model, nil if unknown.
	Model *gpu.Model
	// CapacityMiB is the memory of one device as the pod sees it (the whole
	// GPU, or its MIG slice). 0 if unknown.
	CapacityMiB float64
	// GPUsPerPod is the number of GPU devices each replica requests.
	GPUsPerPod int
	// Sharing is the sharing mode already in place.
	Sharing Sharing
	// MIGProfile is the profile in use when Sharing is SharingMIG.
	MIGProfile string

	// HasMemory and HasUtil say which metrics were found.
	HasMemory bool
	HasUtil   bool
	// PeakMemMiB is the highest framebuffer usage of any single device.
	PeakMemMiB float64
	// AvgUtil, P95Util and MaxUtil are GPU utilization in percent. P95 and
	// max are the worst device of the workload, avg is the mean.
	AvgUtil float64
	P95Util float64
	MaxUtil float64
	// RecentMaxUtil is the highest utilization seen during the last
	// Options.IdleAfter; HasRecent says whether it is known.
	RecentMaxUtil float64
	HasRecent     bool
	// Observed is how long metrics cover the workload inside the window.
	Observed time.Duration
	// IdleFor is how long utilization has stayed at or below the idle
	// threshold. NeverActive is set when no activity was seen in the whole
	// window, in which case IdleFor is a lower bound.
	IdleFor     time.Duration
	NeverActive bool

	// HourlyPrice is the price of one whole GPU of Model; 0 if unknown.
	HourlyPrice float64
	// PriceOf returns the hourly price of another model, for downsizing.
	PriceOf func(*gpu.Model) (float64, bool)
}

// Options tune the engine.
type Options struct {
	// Headroom is the safety margin added to peak memory (0.2 = 20%).
	Headroom float64
	// IdleUtil is the utilization (percent) at or below which a GPU counts
	// as idle.
	IdleUtil float64
	// IdleAfter is how long a GPU must stay idle before it is reported.
	IdleAfter time.Duration
	// MinObservation is the minimum metric coverage needed before anything
	// other than "keep" is recommended.
	MinObservation time.Duration
	// MaxTimeSlice caps how many workloads the engine proposes to pack on
	// one time-sliced GPU.
	MaxTimeSlice int
	// Strategies is the set of enabled optional strategies. Nil enables
	// all of them.
	Strategies []string
}

// DefaultOptions returns the defaults used by the CLI.
func DefaultOptions() Options {
	return Options{
		Headroom:       0.20,
		IdleUtil:       5,
		IdleAfter:      24 * time.Hour,
		MinObservation: time.Hour,
		MaxTimeSlice:   4,
	}
}

// Validate checks the options for nonsensical values.
func (o Options) Validate() error {
	if o.Headroom < 0 || o.Headroom > 5 {
		return fmt.Errorf("headroom must be between 0 and 5 (0.2 = 20%%), got %v", o.Headroom)
	}
	if o.IdleUtil < 0 || o.IdleUtil >= 100 {
		return fmt.Errorf("idle utilization threshold must be in [0, 100), got %v", o.IdleUtil)
	}
	if o.IdleAfter <= 0 {
		return fmt.Errorf("idle-after must be positive")
	}
	if o.MaxTimeSlice < 2 {
		return fmt.Errorf("max time-slice replicas must be at least 2, got %d", o.MaxTimeSlice)
	}
	for _, s := range o.Strategies {
		if !contains(AllStrategies, s) {
			return fmt.Errorf("unknown strategy %q (known: %s)", s, strings.Join(AllStrategies, ", "))
		}
	}
	return nil
}

func (o Options) enabled(strategy string) bool {
	return o.Strategies == nil || contains(o.Strategies, strategy)
}

// Result is the engine's verdict.
type Result struct {
	Action Action `json:"action"`
	// Summary is the short text shown in the table.
	Summary string `json:"summary"`
	// Reason explains the decision in a sentence or two.
	Reason string `json:"reason"`
	// Target is the proposed MIG profile or GPU model key, if any.
	Target string `json:"target,omitempty"`
	// TargetCapacityMiB is the device memory available under the
	// recommendation; 0 when the GPU is kept or reclaimed.
	TargetCapacityMiB float64 `json:"targetCapacityMiB,omitempty"`
	// RequiredMiB is peak memory plus headroom.
	RequiredMiB float64 `json:"requiredMiB,omitempty"`
	// KeepFraction is the share of today's GPU spend still needed under
	// the recommendation (1 = no savings, 0 = everything can go).
	KeepFraction float64 `json:"keepFraction"`
	// Alternatives lists other options that would also fit.
	Alternatives []string `json:"alternatives,omitempty"`
}

type candidate struct {
	action   Action
	summary  string
	reason   string
	target   string
	capacity float64
	keep     float64
}

// Recommend decides what to do with one workload.
func Recommend(in Input, opts Options) Result {
	if !in.HasMemory && !in.HasUtil {
		return Result{Action: ActionUnknown, Summary: "no data", KeepFraction: 1,
			Reason: "No DCGM metrics were found for this workload's pods in the window. Check that dcgm-exporter runs on its nodes and exports pod labels."}
	}
	if in.Observed < opts.MinObservation {
		return Result{Action: ActionUnknown, Summary: "not enough data", KeepFraction: 1,
			Reason: fmt.Sprintf("Only %s of metrics so far; at least %s is needed before making a recommendation.", FormatDuration(in.Observed), FormatDuration(opts.MinObservation))}
	}

	required := in.PeakMemMiB * (1 + opts.Headroom)
	res := Result{Action: ActionKeep, Summary: "keep as is", KeepFraction: 1}
	if in.HasMemory {
		res.RequiredMiB = required
	}

	if !in.HasUtil {
		res.Reason = "Memory usage is known but GPU utilization is not reported for this device (common for MIG instances), so there is not enough evidence to change anything."
		return res
	}

	if idle, reason := isIdle(in, opts); idle {
		dur := FormatDuration(in.IdleFor)
		if in.NeverActive {
			// Idle for the whole observed period: a lower bound.
			if num, unit, ok := strings.Cut(dur, " "); ok {
				dur = num + "+ " + unit
			} else {
				dur += "+"
			}
		}
		return Result{Action: ActionReclaim, Summary: fmt.Sprintf("idle %s, reclaim", dur), Reason: reason, KeepFraction: 0, RequiredMiB: res.RequiredMiB}
	}

	switch in.Sharing {
	case SharingMIG:
		res.Reason = fmt.Sprintf("Already runs on a MIG slice%s and is in use (p95 utilization %.0f%%).", profileSuffix(in.MIGProfile), in.P95Util)
		return res
	case SharingTimeSlice, SharingMPS:
		res.Reason = fmt.Sprintf("Already shares its GPU via %s and is in use (p95 utilization %.0f%%).", in.Sharing, in.P95Util)
		return res
	}

	if !in.HasMemory {
		res.Reason = "GPU utilization is known but memory usage is not, so no smaller option can be verified to fit."
		return res
	}

	if in.GPUsPerPod > 1 {
		if in.P95Util < reviewP95Util {
			res.Action = ActionReview
			res.Summary = "low utilization, review"
			res.Reason = fmt.Sprintf("Each replica holds %d GPUs but p95 utilization is only %.0f%% (avg %.0f%%). Multi-GPU jobs cannot be sliced safely, so no automatic target is proposed; check whether fewer GPUs would do.", in.GPUsPerPod, in.P95Util, in.AvgUtil)
			return res
		}
		res.Reason = fmt.Sprintf("Multi-GPU workload in active use (avg utilization %.0f%%, p95 %.0f%%).", in.AvgUtil, in.P95Util)
		return res
	}

	var cands []candidate
	var notes []string
	finders := map[string]func(Input, Options, float64) (*candidate, string){
		StrategyMIG:       migCandidate,
		StrategyTimeSlice: timeSliceCandidate,
		StrategyDownsize:  downsizeCandidate,
	}
	for _, strategy := range AllStrategies {
		if !opts.enabled(strategy) {
			continue
		}
		if c, note := finders[strategy](in, opts, required); c != nil {
			cands = append(cands, *c)
		} else if note != "" {
			notes = append(notes, note)
		}
	}

	if len(cands) == 0 {
		res.Reason = fmt.Sprintf("Peak memory %s (+%.0f%% headroom = %s), avg utilization %.0f%%, p95 %.0f%%. No smaller option fits safely",
			FormatMiB(in.PeakMemMiB), opts.Headroom*100, FormatMiB(required), in.AvgUtil, in.P95Util)
		if len(notes) > 0 {
			res.Reason += ": " + strings.Join(notes, "; ")
		}
		res.Reason += "."
		return res
	}

	// Candidates are already in order of preference.
	best := cands[0]
	res = Result{
		Action:            best.action,
		Summary:           best.summary,
		Reason:            best.reason,
		Target:            best.target,
		TargetCapacityMiB: best.capacity,
		RequiredMiB:       required,
		KeepFraction:      best.keep,
	}
	for _, c := range cands[1:] {
		res.Alternatives = append(res.Alternatives, c.summary)
	}
	return res
}

func isIdle(in Input, opts Options) (bool, string) {
	if !in.HasRecent || in.RecentMaxUtil > opts.IdleUtil {
		return false, ""
	}
	if in.IdleFor < opts.IdleAfter || in.Observed < opts.IdleAfter {
		return false, ""
	}
	since := fmt.Sprintf("for %s", FormatDuration(in.IdleFor))
	if in.NeverActive {
		since = fmt.Sprintf("for the whole %s it was observed", FormatDuration(in.IdleFor))
	}
	reason := fmt.Sprintf("GPU utilization has not exceeded %.0f%% %s", opts.IdleUtil, since)
	if in.HasMemory && in.PeakMemMiB > 0 {
		reason += fmt.Sprintf(" (it still holds up to %s of GPU memory)", FormatMiB(in.PeakMemMiB))
	}
	return true, reason + ". The GPU can be reclaimed, e.g. by scaling the workload to zero."
}

func migCandidate(in Input, opts Options, required float64) (*candidate, string) {
	if in.Model == nil || !in.Model.SupportsMIG() {
		return nil, ""
	}
	for _, p := range in.Model.MIG {
		if p.IsFull() {
			continue
		}
		if p.MemoryMiB() < required {
			continue
		}
		if in.P95Util > p.ComputeFraction()*100*computeMargin {
			continue
		}
		return &candidate{
			action:   ActionMIG,
			summary:  fmt.Sprintf("MIG %s slice", p.Name),
			target:   p.Name,
			capacity: p.MemoryMiB(),
			keep:     p.Share(),
			reason: fmt.Sprintf("Peak memory %s (+%.0f%% headroom = %s) fits the %s slice of the %s, and p95 utilization %.0f%% fits its %d/%d of compute. Up to %d such slices share one GPU.",
				FormatMiB(in.PeakMemMiB), opts.Headroom*100, FormatMiB(required), p.Name, in.Model.Display, in.P95Util, p.ComputeSlices, p.ComputeTotal, p.MaxInstances),
		}, ""
	}
	return nil, fmt.Sprintf("no partial MIG profile of the %s covers both %s of memory and p95 utilization %.0f%%", in.Model.Display, FormatMiB(required), in.P95Util)
}

func downsizeCandidate(in Input, _ Options, required float64) (*candidate, string) {
	if in.Model == nil || in.HourlyPrice <= 0 || in.PriceOf == nil {
		return nil, ""
	}
	var best *gpu.Model
	bestPrice := in.HourlyPrice
	bestUtil := 0.0
	for _, m := range gpu.SuggestTargets() {
		if m.Family == in.Model.Family {
			continue
		}
		price, ok := in.PriceOf(m)
		if !ok || price <= 0 || price >= bestPrice {
			continue
		}
		if m.MemoryMiB() < required {
			continue
		}
		projected := in.P95Util * in.Model.Throughput / m.Throughput
		if projected > downsizeMaxUtil {
			continue
		}
		best, bestPrice, bestUtil = m, price, projected
	}
	if best == nil {
		return nil, "no cheaper GPU model has enough memory and compute"
	}
	return &candidate{
		action:   ActionDownsize,
		summary:  fmt.Sprintf("smaller GPU: %s (%d GB)", best.Short, best.MemoryGB),
		target:   best.Key,
		capacity: best.MemoryMiB(),
		keep:     bestPrice / in.HourlyPrice,
		reason: fmt.Sprintf("Peak memory %s (+headroom = %s) fits the %d GB of the %s, and p95 utilization %.0f%% on the %s is roughly %.0f%% on the %s (coarse cross-model estimate, capped at %.0f%%).",
			FormatMiB(in.PeakMemMiB), FormatMiB(required), best.MemoryGB, best.Display, in.P95Util, in.Model.Display, bestUtil, best.Display, downsizeMaxUtil),
	}, ""
}

func timeSliceCandidate(in Input, opts Options, required float64) (*candidate, string) {
	if in.CapacityMiB <= 0 {
		return nil, ""
	}
	n := opts.MaxTimeSlice
	if required > 0 {
		n = minInt(n, int(math.Floor(in.CapacityMiB/required)))
	}
	if in.P95Util > 0 {
		n = minInt(n, int(math.Floor(100*computeMargin/in.P95Util)))
	}
	if n < 2 {
		return nil, "the GPU is too full to share via time-slicing"
	}
	return &candidate{
		action:   ActionTimeSlice,
		summary:  fmt.Sprintf("time-slice, %d per GPU", n),
		target:   fmt.Sprintf("%d", n),
		capacity: in.CapacityMiB / float64(n),
		keep:     1 / float64(n),
		reason: fmt.Sprintf("Peak memory %s (+headroom = %s) and p95 utilization %.0f%% leave room for %d such workloads on one %s GPU via time-slicing. Time-slicing has no memory isolation, so only co-locate workloads you trust.",
			FormatMiB(in.PeakMemMiB), FormatMiB(required), in.P95Util, n, FormatMiB(in.CapacityMiB)),
	}, ""
}

func profileSuffix(profile string) string {
	if profile == "" {
		return ""
	}
	return " (" + profile + ")"
}

// FormatMiB renders a MiB quantity the way nvidia-smi users expect:
// binary gigabytes labelled "GB".
func FormatMiB(mib float64) string {
	gb := mib / 1024
	switch {
	case gb >= 9.95:
		return fmt.Sprintf("%.0f GB", gb)
	default:
		return fmt.Sprintf("%.1f GB", gb)
	}
}

// FormatDuration renders a duration coarsely: "45m", "9h", "9 days".
func FormatDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func contains(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
