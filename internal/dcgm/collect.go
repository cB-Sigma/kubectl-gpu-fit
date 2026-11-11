// Package dcgm turns the NVIDIA DCGM exporter metrics stored in Prometheus
// into per-pod, per-device usage summaries.
package dcgm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/prom"
)

// Querier runs a PromQL instant query. *prom.Client implements it.
type Querier interface {
	Query(ctx context.Context, query string, at time.Time) ([]prom.Sample, error)
}

// Usage summarizes one GPU device (or MIG instance) as used by one pod
// container over the query window.
type Usage struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
	// Device identifies the GPU: its UUID, plus the MIG instance ID if any.
	Device string `json:"device"`
	// Host is the dcgm-exporter "Hostname" label, informational only.
	Host string `json:"host,omitempty"`
	// ModelName is the raw DCGM model string, e.g. "NVIDIA A100-SXM4-80GB".
	ModelName string `json:"modelName,omitempty"`
	// MIGProfile is set for MIG instances, e.g. "1g.10gb".
	MIGProfile string `json:"migProfile,omitempty"`

	HasUtil bool `json:"hasUtil"`
	// Utilization figures are percentages (0-100).
	UtilAvg float64 `json:"utilAvg"`
	UtilP95 float64 `json:"utilP95"`
	UtilMax float64 `json:"utilMax"`
	// UtilRecentMax is the maximum over the trailing "recent" window.
	HasRecent     bool    `json:"hasRecent"`
	UtilRecentMax float64 `json:"utilRecentMax"`
	// LastActive is the last time utilization exceeded the idle threshold;
	// zero if that never happened inside the window.
	LastActive time.Time `json:"lastActive,omitempty"`

	HasMemory bool `json:"hasMemory"`
	// Memory figures are MiB.
	MemPeakMiB float64 `json:"memPeakMiB"`
	MemAvgMiB  float64 `json:"memAvgMiB"`
	// CapacityMiB is the framebuffer size of the device, 0 if unknown.
	CapacityMiB float64 `json:"capacityMiB"`

	// Observed is how long the series exists inside the window.
	Observed time.Duration `json:"observed"`

	freeAvg, reservedAvg float64
	primaryUtil          bool
}

// Options configure Collect.
type Options struct {
	// Window is the look-back period, ending at Now.
	Window time.Duration
	// Recent is the trailing period used for the idle check.
	Recent time.Duration
	// Step is the subquery resolution for coverage and last-activity.
	Step time.Duration
	// IdleUtil is the utilization percentage above which a GPU is active.
	IdleUtil float64
	// Namespace restricts results to one namespace; empty means all.
	Namespace string
	// Now is the evaluation time.
	Now time.Time
	// Concurrency bounds parallel queries (default 6).
	Concurrency int
}

// Stats describes what Collect saw, for diagnostics.
type Stats struct {
	// Queries is the number of PromQL queries issued.
	Queries int
	// Series is the number of DCGM series returned by all queries.
	Series int
	// Unattributed is how many of them carried no pod labels.
	Unattributed int
	// Metrics lists the metric names that returned data.
	Metrics []string
}

// metric is one spelling of a DCGM quantity. dcgm-exporter 4.9 renamed the
// default metrics and changed their units; both generations are queried.
type metric struct {
	name string
	// scale converts the stored value to the canonical unit (percent for
	// utilization, MiB for memory).
	scale float64
}

const bytesToMiB = 1.0 / (1 << 20)

var (
	utilMetrics = []metric{
		{"DCGM_FI_DEV_GPU_UTIL", 1},         // percent
		{"DCGM_FI_DEV_GPU_UTIL_RATIO", 100}, // ratio, dcgm-exporter >= 4.9
	}
	// DCGM does not report DCGM_FI_DEV_GPU_UTIL for MIG instances; the
	// profiling "graphics engine active" ratio is the closest substitute.
	utilFallbackMetrics = []metric{
		{"DCGM_FI_PROF_GR_ENGINE_ACTIVE", 100},
		{"DCGM_FI_PROF_GR_ENGINE_UTIL_RATIO", 100}, // dcgm-exporter >= 4.9
	}
	memUsedMetrics = []metric{
		{"DCGM_FI_DEV_FB_USED", 1}, // MiB
		{"DCGM_FI_DEV_FB_USED_BYTES", bytesToMiB},
	}
	memFreeMetrics = []metric{
		{"DCGM_FI_DEV_FB_FREE", 1},
		{"DCGM_FI_DEV_FB_FREE_BYTES", bytesToMiB},
	}
	memReservedMetrics = []metric{
		{"DCGM_FI_DEV_FB_RESERVED", 1},
		{"DCGM_FI_DEV_FB_RESERVED_BYTES", bytesToMiB},
	}
	memCapacityMetrics = []metric{
		{"DCGM_FI_DEV_FB_CAPACITY_BYTES", bytesToMiB},
	}
)

// MetricNames lists every metric name Collect may query.
func MetricNames() []string {
	var out []string
	for _, group := range [][]metric{utilMetrics, utilFallbackMetrics, memUsedMetrics, memFreeMetrics, memReservedMetrics, memCapacityMetrics} {
		for _, m := range group {
			out = append(out, m.name)
		}
	}
	return out
}

type job struct {
	metric metric
	query  string
	apply  func(u *Usage, v float64, s prom.Sample)
}

// Collect queries Prometheus and returns one Usage per (pod, container,
// device), sorted by namespace, pod and device.
func Collect(ctx context.Context, q Querier, opts Options) ([]Usage, Stats, error) {
	if opts.Window <= 0 {
		return nil, Stats{}, fmt.Errorf("window must be positive")
	}
	if opts.Step <= 0 {
		opts.Step = 5 * time.Minute
	}
	if opts.Step > opts.Window {
		opts.Step = opts.Window
	}
	if opts.Recent <= 0 || opts.Recent > opts.Window {
		opts.Recent = opts.Window
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 6
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	c := &collector{opts: opts, q: q, usage: map[string]*Usage{}, seen: map[string]bool{}}

	var jobs []job
	for _, m := range utilMetrics {
		jobs = append(jobs, utilJobs(m, opts)...)
	}
	for _, m := range memUsedMetrics {
		jobs = append(jobs, memJobs(m, opts)...)
	}
	for _, m := range memFreeMetrics {
		jobs = append(jobs, job{m, overTime("avg_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.freeAvg = v }})
	}
	for _, m := range memReservedMetrics {
		jobs = append(jobs, job{m, overTime("avg_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.reservedAvg = v }})
	}
	for _, m := range memCapacityMetrics {
		jobs = append(jobs, job{m, overTime("max_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.CapacityMiB = v }})
	}
	if err := c.run(ctx, jobs); err != nil {
		return nil, c.stats(), err
	}

	// Second pass: only when some device has memory data but no
	// utilization (typically MIG instances), try the profiling metrics.
	needFallback := false
	for _, u := range c.usage {
		if u.HasMemory && !u.HasUtil {
			needFallback = true
			break
		}
	}
	if needFallback {
		jobs = jobs[:0]
		for _, m := range utilFallbackMetrics {
			for _, j := range utilJobs(m, opts) {
				apply := j.apply
				j.apply = func(u *Usage, v float64, s prom.Sample) {
					if !u.primaryUtil {
						apply(u, v, s)
					}
				}
				jobs = append(jobs, j)
			}
		}
		// Mark devices that already have utilization so the fallback
		// does not overwrite them.
		for _, u := range c.usage {
			u.primaryUtil = u.HasUtil
		}
		if err := c.run(ctx, jobs); err != nil {
			return nil, c.stats(), err
		}
	}

	out := make([]Usage, 0, len(c.usage))
	for _, u := range c.usage {
		if u.HasMemory && u.CapacityMiB == 0 && u.freeAvg > 0 {
			// used + free (+ reserved) is constant, so the sum of the
			// averages is the device's framebuffer size.
			u.CapacityMiB = u.MemAvgMiB + u.freeAvg + u.reservedAvg
		}
		if u.Observed > opts.Window {
			u.Observed = opts.Window
		}
		if !u.HasUtil {
			u.LastActive = time.Time{}
			u.HasRecent = false
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		return a.Device < b.Device
	})
	return out, c.stats(), nil
}

func utilJobs(m metric, opts Options) []job {
	threshold := opts.IdleUtil / m.scale
	return []job{
		{m, overTime("avg_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.UtilAvg, u.HasUtil = v, true }},
		{m, overTime("max_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.UtilMax, u.HasUtil = v, true }},
		{m, fmt.Sprintf("quantile_over_time(0.95, %s[%s])", m.name, promDuration(opts.Window)), func(u *Usage, v float64, _ prom.Sample) { u.UtilP95, u.HasUtil = v, true }},
		{m, overTime("max_over_time", m.name, opts.Recent), func(u *Usage, v float64, _ prom.Sample) { u.UtilRecentMax, u.HasRecent = v, true }},
		{
			// The value is a Unix timestamp and must not be scaled.
			metric{m.name, 1},
			fmt.Sprintf("max_over_time(timestamp(%s > %s)[%s:%s])", m.name, formatFloat(threshold), promDuration(opts.Window), promDuration(opts.Step)),
			func(u *Usage, v float64, _ prom.Sample) { u.LastActive = time.Unix(int64(v), 0).UTC() },
		},
	}
}

func memJobs(m metric, opts Options) []job {
	step := opts.Step
	return []job{
		{m, overTime("max_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.MemPeakMiB, u.HasMemory = v, true }},
		{m, overTime("avg_over_time", m.name, opts.Window), func(u *Usage, v float64, _ prom.Sample) { u.MemAvgMiB, u.HasMemory = v, true }},
		{
			// The value is a sample count and must not be scaled.
			metric{m.name, 1},
			fmt.Sprintf("count_over_time(%s[%s:%s])", m.name, promDuration(opts.Window), promDuration(step)),
			func(u *Usage, v float64, _ prom.Sample) { u.Observed = time.Duration(v) * step },
		},
	}
}

type collector struct {
	opts Options
	q    Querier

	mu           sync.Mutex
	usage        map[string]*Usage
	seen         map[string]bool
	queries      int
	series       int
	unattributed int
}

func (c *collector) stats() Stats {
	names := make([]string, 0, len(c.seen))
	for n := range c.seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return Stats{Queries: c.queries, Series: c.series, Unattributed: c.unattributed, Metrics: names}
}

// run executes the jobs with bounded parallelism. Results are applied in
// job order so that the outcome does not depend on scheduling.
func (c *collector) run(ctx context.Context, jobs []job) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([][]prom.Sample, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, c.opts.Concurrency)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			res, err := c.q.Query(ctx, jobs[i].query, c.opts.Now)
			if err != nil {
				errs[i] = fmt.Errorf("query %q: %w", jobs[i].query, err)
				cancel()
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	// Report the first real failure rather than a follow-on cancellation.
	var firstErr error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if firstErr == nil || (strings.Contains(firstErr.Error(), context.Canceled.Error()) && !strings.Contains(err.Error(), context.Canceled.Error())) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries += len(jobs)
	for i, j := range jobs {
		if len(results[i]) > 0 {
			c.seen[j.metric.name] = true
		}
		for _, s := range results[i] {
			c.series++
			ref, ok := PodRefFromLabels(s.Labels)
			if !ok {
				c.unattributed++
				continue
			}
			if c.opts.Namespace != "" && ref.Namespace != c.opts.Namespace {
				continue
			}
			device := deviceID(s.Labels)
			key := ref.Namespace + "/" + ref.Pod + "/" + ref.Container + "/" + device
			u := c.usage[key]
			if u == nil {
				u = &Usage{
					Namespace:  ref.Namespace,
					Pod:        ref.Pod,
					Container:  ref.Container,
					Device:     device,
					Host:       firstLabel(s.Labels, "Hostname", "hostname"),
					ModelName:  firstLabel(s.Labels, "modelName", "model_name"),
					MIGProfile: firstLabel(s.Labels, "GPU_I_PROFILE"),
				}
				c.usage[key] = u
			}
			j.apply(u, s.Value*j.metric.scale, s)
		}
	}
	return nil
}

// PodRef names the pod container a DCGM series is attributed to.
type PodRef struct {
	Namespace string
	Pod       string
	Container string
}

// PodRefFromLabels extracts the workload pod from a DCGM series.
//
// dcgm-exporter attaches "pod", "namespace" and "container" labels for the
// pod using the GPU. When Prometheus scrapes it without honorLabels, those
// collide with the target labels of the exporter pod itself and are stored
// as "exported_pod", "exported_namespace" and "exported_container"; in that
// case the plain labels describe the exporter, not the workload. Very old
// exporters (or --use-old-namespace) use "pod_name", "pod_namespace" and
// "container_name".
func PodRefFromLabels(l map[string]string) (PodRef, bool) {
	if pod := l["exported_pod"]; pod != "" {
		if ns := l["exported_namespace"]; ns != "" {
			return PodRef{Namespace: ns, Pod: pod, Container: l["exported_container"]}, true
		}
		return PodRef{}, false
	}
	if pod := l["pod"]; pod != "" {
		if ns := l["namespace"]; ns != "" {
			return PodRef{Namespace: ns, Pod: pod, Container: l["container"]}, true
		}
	}
	if pod := l["pod_name"]; pod != "" {
		if ns := l["pod_namespace"]; ns != "" {
			return PodRef{Namespace: ns, Pod: pod, Container: l["container_name"]}, true
		}
	}
	return PodRef{}, false
}

func deviceID(l map[string]string) string {
	id := firstLabel(l, "UUID", "uuid", "gpu_uuid")
	if id == "" {
		id = firstLabel(l, "Hostname", "hostname", "instance") + "/gpu" + l["gpu"]
	}
	if inst := l["GPU_I_ID"]; inst != "" {
		id += "/mig" + inst
	}
	return id
}

func firstLabel(l map[string]string, names ...string) string {
	for _, n := range names {
		if v := l[n]; v != "" {
			return v
		}
	}
	return ""
}

func overTime(fn, metricName string, window time.Duration) string {
	return fmt.Sprintf("%s(%s[%s])", fn, metricName, promDuration(window))
}

// promDuration renders a duration in PromQL syntax using whole seconds or a
// larger unit when it divides evenly.
func promDuration(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs <= 0 {
		secs = 1
	}
	switch {
	case secs%86400 == 0:
		return fmt.Sprintf("%dd", secs/86400)
	case secs%3600 == 0:
		return fmt.Sprintf("%dh", secs/3600)
	case secs%60 == 0:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
