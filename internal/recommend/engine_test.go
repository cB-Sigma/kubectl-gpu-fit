package recommend

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/pricing"
)

const day = 24 * time.Hour

func model(t testing.TB, key string) *gpu.Model {
	t.Helper()
	m, ok := gpu.ByKey(key)
	if !ok {
		t.Fatalf("no model %s", key)
	}
	return m
}

// busy returns an input for a single whole GPU observed for a week.
func base(t testing.TB, key string, peakGiB, avg, p95 float64) Input {
	m := model(t, key)
	prices := pricing.Defaults()
	price, _ := prices.Lookup(m)
	return Input{
		Model:         m,
		CapacityMiB:   float64(m.MemoryGB) * 1024,
		GPUsPerPod:    1,
		HasMemory:     true,
		HasUtil:       true,
		PeakMemMiB:    peakGiB * 1024,
		AvgUtil:       avg,
		P95Util:       p95,
		MaxUtil:       math.Min(100, p95+10),
		RecentMaxUtil: math.Min(100, p95+10),
		HasRecent:     true,
		Observed:      7 * day,
		HourlyPrice:   price,
		PriceOf:       prices.Lookup,
	}
}

func TestRecommend(t *testing.T) {
	opts := DefaultOptions()
	tests := []struct {
		name        string
		in          func() Input
		opts        func(*Options)
		wantAction  Action
		wantSummary string
		wantTarget  string
		wantKeep    float64
	}{
		{
			name:       "no metrics at all",
			in:         func() Input { return Input{GPUsPerPod: 1} },
			wantAction: ActionUnknown, wantSummary: "no data", wantKeep: 1,
		},
		{
			name: "too little data",
			in: func() Input {
				in := base(t, "a100-80gb", 2, 1, 2)
				in.Observed = 20 * time.Minute
				return in
			},
			wantAction: ActionUnknown, wantSummary: "not enough data", wantKeep: 1,
		},
		{
			name:       "small inference service fits smallest A100 slice",
			in:         func() Input { return base(t, "a100-80gb", 6.1, 9, 12) },
			wantAction: ActionMIG, wantSummary: "MIG 1g.10gb slice", wantTarget: "1g.10gb", wantKeep: 1.0 / 7,
		},
		{
			name:       "memory just over the slice once headroom is added",
			in:         func() Input { return base(t, "a100-80gb", 8.0, 5, 8) },
			wantAction: ActionMIG, wantSummary: "MIG 1g.20gb slice", wantTarget: "1g.20gb", wantKeep: 1.0 / 4,
		},
		{
			name:       "compute needs two slices",
			in:         func() Input { return base(t, "a100-80gb", 4, 15, 22) },
			wantAction: ActionMIG, wantSummary: "MIG 2g.20gb slice", wantTarget: "2g.20gb", wantKeep: 2.0 / 7,
		},
		{
			name:       "40GB A100 uses its own profile names",
			in:         func() Input { return base(t, "a100-40gb", 3, 4, 6) },
			wantAction: ActionMIG, wantSummary: "MIG 1g.5gb slice", wantTarget: "1g.5gb", wantKeep: 1.0 / 7,
		},
		{
			name:       "H200 slice",
			in:         func() Input { return base(t, "h200", 20, 5, 9) },
			wantAction: ActionMIG, wantSummary: "MIG 1g.35gb slice", wantTarget: "1g.35gb", wantKeep: 1.0 / 4,
		},
		{
			name:       "A30 half",
			in:         func() Input { return base(t, "a30", 7, 20, 40) },
			opts:       func(o *Options) { o.Strategies = []string{StrategyMIG} },
			wantAction: ActionMIG, wantSummary: "MIG 2g.12gb slice", wantTarget: "2g.12gb", wantKeep: 0.5,
		},
		{
			name:       "busy training job keeps its GPU",
			in:         func() Input { return base(t, "h100", 71, 88, 99) },
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name:       "low util but memory needs the whole card",
			in:         func() Input { return base(t, "a100-80gb", 70, 5, 8) },
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "multi-GPU busy job is kept",
			in: func() Input {
				in := base(t, "h100", 71, 88, 97)
				in.GPUsPerPod = 4
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "multi-GPU quiet job is only flagged",
			in: func() Input {
				in := base(t, "h100", 10, 12, 30)
				in.GPUsPerPod = 4
				return in
			},
			wantAction: ActionReview, wantSummary: "low utilization, review", wantKeep: 1,
		},
		{
			name: "idle notebook",
			in: func() Input {
				in := base(t, "a100-80gb", 0.3, 0, 0)
				in.MaxUtil, in.RecentMaxUtil = 0, 0
				in.Observed = 14 * day
				in.IdleFor = 9 * day
				return in
			},
			wantAction: ActionReclaim, wantSummary: "idle 9 days, reclaim", wantKeep: 0,
		},
		{
			name: "idle for the whole window",
			in: func() Input {
				in := base(t, "t4", 0.1, 0, 0)
				in.MaxUtil, in.RecentMaxUtil = 0, 0
				in.IdleFor, in.NeverActive = 7*day, true
				return in
			},
			wantAction: ActionReclaim, wantSummary: "idle 7+ days, reclaim", wantKeep: 0,
		},
		{
			name: "idle multi-GPU job",
			in: func() Input {
				in := base(t, "h100", 1, 0, 0)
				in.GPUsPerPod = 8
				in.MaxUtil, in.RecentMaxUtil = 2, 1
				in.IdleFor = 30 * time.Hour
				return in
			},
			wantAction: ActionReclaim, wantSummary: "idle 30h, reclaim", wantKeep: 0,
		},
		{
			name: "quiet but not for long enough is not idle",
			in: func() Input {
				in := base(t, "a100-80gb", 70, 30, 60)
				in.RecentMaxUtil = 0
				in.IdleFor = 3 * time.Hour
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "recent activity beats a stale idle estimate",
			in: func() Input {
				in := base(t, "a100-80gb", 70, 1, 2)
				in.RecentMaxUtil = 80
				in.IdleFor = 3 * day
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "no recent data is not idle",
			in: func() Input {
				in := base(t, "a100-80gb", 70, 0, 0)
				in.HasRecent = false
				in.IdleFor = 3 * day
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name:       "T4 has no MIG: time-slice",
			in:         func() Input { return base(t, "t4", 2, 4, 9) },
			wantAction: ActionTimeSlice, wantSummary: "time-slice, 4 per GPU", wantTarget: "4", wantKeep: 0.25,
		},
		{
			name:       "time-slice count limited by memory",
			in:         func() Input { return base(t, "t4", 5, 4, 9) },
			wantAction: ActionTimeSlice, wantSummary: "time-slice, 2 per GPU", wantTarget: "2", wantKeep: 0.5,
		},
		{
			name:       "time-slice count limited by compute",
			in:         func() Input { return base(t, "t4", 1, 20, 30) },
			wantAction: ActionTimeSlice, wantSummary: "time-slice, 3 per GPU", wantTarget: "3", wantKeep: 1.0 / 3,
		},
		{
			name:       "T4 too full to share",
			in:         func() Input { return base(t, "t4", 9, 30, 50) },
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name:       "MIG is preferred even when another model would be cheaper",
			in:         func() Input { return base(t, "a100-80gb", 30, 5, 10) },
			wantAction: ActionMIG, wantSummary: "MIG 3g.40gb slice", wantTarget: "3g.40gb", wantKeep: 0.5,
		},
		{
			name:       "downsize only",
			in:         func() Input { return base(t, "a100-80gb", 30, 5, 10) },
			opts:       func(o *Options) { o.Strategies = []string{StrategyDownsize} },
			wantAction: ActionDownsize, wantSummary: "smaller GPU: L40S (48 GB)", wantTarget: "l40s", wantKeep: 1.86 / 5.07,
		},
		{
			name:       "time-slicing is preferred over a different model",
			in:         func() Input { return base(t, "l40s", 16, 4, 10) },
			wantAction: ActionTimeSlice, wantSummary: "time-slice, 2 per GPU", wantTarget: "2", wantKeep: 0.5,
		},
		{
			name:       "too much memory to share falls through to a cheaper card",
			in:         func() Input { return base(t, "a100-80gb", 35, 5, 10) },
			opts:       func(o *Options) { o.Strategies = []string{StrategyTimeSlice, StrategyDownsize} },
			wantAction: ActionDownsize, wantSummary: "smaller GPU: L40S (48 GB)", wantTarget: "l40s", wantKeep: 1.86 / 5.07,
		},
		{
			name:       "all strategies disabled",
			in:         func() Input { return base(t, "a100-80gb", 2, 1, 2) },
			opts:       func(o *Options) { o.Strategies = []string{} },
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "existing MIG slice in use is kept",
			in: func() Input {
				in := base(t, "a100-80gb", 3, 20, 40)
				in.Sharing, in.MIGProfile, in.CapacityMiB = SharingMIG, "3g.40gb", 40192
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "MIG slice without utilization metrics is kept",
			in: func() Input {
				in := base(t, "a100-80gb", 0, 0, 0)
				in.Sharing, in.MIGProfile, in.HasUtil, in.HasRecent = SharingMIG, "1g.10gb", false, false
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "idle time-sliced replica is reclaimed",
			in: func() Input {
				in := base(t, "t4", 1, 0, 0)
				in.Sharing = SharingTimeSlice
				in.MaxUtil, in.RecentMaxUtil = 0, 0
				in.IdleFor = 5 * day
				return in
			},
			wantAction: ActionReclaim, wantSummary: "idle 5 days, reclaim", wantKeep: 0,
		},
		{
			name: "time-sliced replica in use is kept",
			in: func() Input {
				in := base(t, "t4", 1, 3, 8)
				in.Sharing = SharingTimeSlice
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "utilization without memory is kept",
			in: func() Input {
				in := base(t, "a100-80gb", 0, 3, 5)
				in.HasMemory = false
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name: "unknown model can still be time-sliced",
			in: func() Input {
				in := base(t, "t4", 2, 3, 5)
				in.Model, in.HourlyPrice, in.CapacityMiB = nil, 0, 24000
				return in
			},
			wantAction: ActionTimeSlice, wantSummary: "time-slice, 4 per GPU", wantTarget: "4", wantKeep: 0.25,
		},
		{
			name: "unknown model and capacity is kept",
			in: func() Input {
				in := base(t, "t4", 2, 3, 5)
				in.Model, in.HourlyPrice, in.CapacityMiB = nil, 0, 0
				return in
			},
			wantAction: ActionKeep, wantSummary: "keep as is", wantKeep: 1,
		},
		{
			name:       "zero headroom lets 9 GB into a 10gb slice",
			in:         func() Input { return base(t, "a100-80gb", 9, 3, 5) },
			opts:       func(o *Options) { o.Headroom = 0 },
			wantAction: ActionMIG, wantSummary: "MIG 1g.10gb slice", wantTarget: "1g.10gb", wantKeep: 1.0 / 7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := opts
			if tt.opts != nil {
				tt.opts(&o)
			}
			got := Recommend(tt.in(), o)
			if got.Action != tt.wantAction || got.Summary != tt.wantSummary || got.Target != tt.wantTarget {
				t.Errorf("got %s / %q / target %q, want %s / %q / target %q\nreason: %s",
					got.Action, got.Summary, got.Target, tt.wantAction, tt.wantSummary, tt.wantTarget, got.Reason)
			}
			if math.Abs(got.KeepFraction-tt.wantKeep) > 1e-9 {
				t.Errorf("KeepFraction = %v, want %v", got.KeepFraction, tt.wantKeep)
			}
			if got.Reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestAlternatives(t *testing.T) {
	got := Recommend(base(t, "a100-80gb", 6.1, 9, 12), DefaultOptions())
	if got.Action != ActionMIG {
		t.Fatalf("action = %s", got.Action)
	}
	joined := strings.Join(got.Alternatives, "|")
	if !strings.Contains(joined, "time-slice") || !strings.Contains(joined, "smaller GPU") {
		t.Errorf("alternatives = %v, want time-slice and downsize options", got.Alternatives)
	}
}

// TestNeverRecommendsWhatDoesNotFit throws random workloads at the engine and
// checks the safety invariants that matter most.
func TestNeverRecommendsWhatDoesNotFit(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	models := gpu.Models()
	prices := pricing.Defaults()
	seen := map[Action]int{}
	for i := 0; i < 50000; i++ {
		m := models[rng.Intn(len(models))]
		capacity := float64(m.MemoryGB) * 1024
		opts := DefaultOptions()
		opts.Headroom = []float64{0, 0.1, 0.2, 0.5}[rng.Intn(4)]
		price, _ := prices.Lookup(m)
		p95 := rng.Float64() * 100
		if rng.Intn(3) == 0 {
			p95 = rng.Float64() * 15
		}
		in := Input{
			Model:         m,
			CapacityMiB:   capacity,
			GPUsPerPod:    1 + rng.Intn(2)*rng.Intn(4),
			HasMemory:     true,
			HasUtil:       true,
			PeakMemMiB:    rng.Float64() * capacity,
			AvgUtil:       p95 * rng.Float64(),
			P95Util:       p95,
			MaxUtil:       p95,
			RecentMaxUtil: p95,
			HasRecent:     true,
			Observed:      time.Duration(rng.Intn(14*24)) * time.Hour,
			HourlyPrice:   price,
			PriceOf:       prices.Lookup,
		}
		if rng.Intn(4) == 0 {
			in.PeakMemMiB = rng.Float64() * 8 * 1024
		}
		if p95 <= opts.IdleUtil {
			in.IdleFor = time.Duration(rng.Float64() * float64(in.Observed))
		}
		got := Recommend(in, opts)
		seen[got.Action]++
		required := in.PeakMemMiB * (1 + opts.Headroom)

		if got.KeepFraction < 0 || got.KeepFraction > 1 {
			t.Fatalf("KeepFraction %v out of range: %+v", got.KeepFraction, got)
		}
		switch got.Action {
		case ActionKeep, ActionUnknown, ActionReview:
			if got.KeepFraction != 1 {
				t.Fatalf("%s must not claim savings: %+v", got.Action, got)
			}
		case ActionReclaim:
			if in.RecentMaxUtil > opts.IdleUtil || in.IdleFor < opts.IdleAfter {
				t.Fatalf("reclaim for a GPU that is not idle: in=%+v got=%+v", in, got)
			}
		case ActionMIG, ActionDownsize, ActionTimeSlice:
			if got.TargetCapacityMiB+1e-6 < required {
				t.Fatalf("target with %.0f MiB cannot hold %.0f MiB: in=%+v got=%+v", got.TargetCapacityMiB, required, in, got)
			}
			if in.GPUsPerPod > 1 {
				t.Fatalf("multi-GPU workload got %s", got.Action)
			}
			if in.Observed < opts.MinObservation {
				t.Fatalf("recommendation from %s of data", in.Observed)
			}
			if got.KeepFraction >= 1 {
				t.Fatalf("recommendation without savings: %+v", got)
			}
		default:
			t.Fatalf("unexpected action %q", got.Action)
		}
		if got.Action == ActionMIG {
			p, ok := m.Profile(got.Target)
			if !ok || p.IsFull() {
				t.Fatalf("%s: bad MIG target %q", m.Key, got.Target)
			}
			if in.P95Util > p.ComputeFraction()*100 {
				t.Fatalf("p95 %.1f%% does not fit %s", in.P95Util, p.Name)
			}
		}
		if got.Action == ActionDownsize {
			target, ok := gpu.ByKey(got.Target)
			if !ok {
				t.Fatalf("bad downsize target %q", got.Target)
			}
			targetPrice, _ := prices.Lookup(target)
			if targetPrice >= price || target.MemoryMiB() < required {
				t.Fatalf("downsize to %s is not smaller/cheaper or does not fit", target.Key)
			}
		}
	}
	for _, a := range []Action{ActionKeep, ActionUnknown, ActionReview, ActionMIG, ActionDownsize, ActionTimeSlice, ActionReclaim} {
		if seen[a] == 0 {
			t.Errorf("random inputs never produced %s; the test lost coverage", a)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := DefaultOptions().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Options){
		func(o *Options) { o.Headroom = -0.1 },
		func(o *Options) { o.Headroom = 20 },
		func(o *Options) { o.IdleUtil = 100 },
		func(o *Options) { o.IdleAfter = 0 },
		func(o *Options) { o.MaxTimeSlice = 1 },
		func(o *Options) { o.Strategies = []string{"magic"} },
	}
	for i, mutate := range bad {
		o := DefaultOptions()
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestFormatters(t *testing.T) {
	for mib, want := range map[float64]string{0: "0.0 GB", 307: "0.3 GB", 6246: "6.1 GB", 72704: "71 GB", 10240: "10 GB"} {
		if got := FormatMiB(mib); got != want {
			t.Errorf("FormatMiB(%v) = %q, want %q", mib, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		-time.Second: "0m", 20 * time.Minute: "20m", 90 * time.Minute: "1h", 47 * time.Hour: "47h", 48 * time.Hour: "2 days", 9*day + time.Hour: "9 days",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
