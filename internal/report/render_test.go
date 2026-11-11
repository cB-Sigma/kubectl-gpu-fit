package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/recommend"
)

func sampleReport() *Report {
	f := func(v float64) *float64 { return &v }
	rows := []Row{
		{
			Namespace: "ml-serving", Workload: "embed-api", Kind: "Deployment", Replicas: 4, GPUs: 4,
			Requested:  Requested{Count: 1, Resource: "nvidia.com/gpu", Model: "a100-80gb", ModelName: "A100 80GB", Text: "1x A100"},
			PeakMemMiB: f(6246), AvgUtil: f(11), P95Util: f(12), MaxUtil: f(40), ObservedSeconds: 7 * 86400,
			Recommendation: recommend.Result{Action: recommend.ActionMIG, Summary: "MIG 1g.10gb slice", Reason: "Fits | the slice.", KeepFraction: 1.0 / 7,
				Alternatives: []string{"time-slice, 4 per GPU"}},
			HourlyPrice: f(3.67), MonthlyCost: 10716.4, MonthlyWaste: 9185.5,
		},
		{
			Namespace: "ml-training", Workload: "train-nightly", Kind: "StatefulSet", Replicas: 1, GPUs: 4,
			Requested:  Requested{Count: 4, Resource: "nvidia.com/gpu", Model: "h100", ModelName: "H100 80GB", Text: "4x H100"},
			PeakMemMiB: f(72704), AvgUtil: f(88), P95Util: f(99), MaxUtil: f(100), ObservedSeconds: 7 * 86400,
			Recommendation: recommend.Result{Action: recommend.ActionKeep, Summary: "keep as is", Reason: "Busy.", KeepFraction: 1},
			HourlyPrice:    f(9.8), MonthlyCost: 28616,
		},
		{
			Namespace: "notebooks", Workload: "notebook-priya", Kind: "Pod", Replicas: 1, GPUs: 1,
			Requested:  Requested{Count: 1, Resource: "nvidia.com/gpu", Model: "a100-80gb", ModelName: "A100 80GB", Text: "1x A100"},
			PeakMemMiB: f(307), AvgUtil: f(0), P95Util: f(0), MaxUtil: f(0), ObservedSeconds: 14 * 86400, IdleSeconds: 9 * 86400,
			Recommendation: recommend.Result{Action: recommend.ActionReclaim, Summary: "idle 9 days, reclaim", Reason: "Idle.", KeepFraction: 0},
			HourlyPrice:    f(3.67), MonthlyCost: 2679.1, MonthlyWaste: 2679.1,
		},
		{
			Namespace: "sandbox", Workload: "mystery", Kind: "Pod", Replicas: 1, GPUs: 1,
			Requested:      Requested{Count: 1, Resource: "nvidia.com/gpu", Text: "1x GPU"},
			Recommendation: recommend.Result{Action: recommend.ActionUnknown, Summary: "no data", Reason: "No metrics.", KeepFraction: 1},
		},
	}
	rep := &Report{Window: "7d", Headroom: 0.2, Namespace: "", Source: "test", Rows: rows, PriceNote: PriceNote}
	rep.Summary = Summarize(rows)
	return rep
}

func render(t *testing.T, rep *Report, opts RenderOptions) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, rep, opts); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRenderTable(t *testing.T) {
	rep := sampleReport()
	rep.Namespace = "ml"
	got := render(t, rep, RenderOptions{Format: "table"})
	want := `WORKLOAD         REQUESTED   PEAK MEM   AVG UTIL   RECOMMENDATION         WASTE/MO
embed-api (x4)   1x A100     6.1 GB     11%        MIG 1g.10gb slice      $9,200
train-nightly    4x H100     71 GB      88%        keep as is             -
notebook-priya   1x A100     0.3 GB     0%         idle 9 days, reclaim   $2,700
mystery          1x GPU      -          -          no data                -

4 workloads, 10 GPUs, est. cost $42,000/mo, est. waste $12,000/mo (28%)
Window 7d, memory headroom 20%, namespace ml. ` + PriceNote + "\n"
	if got != want {
		t.Errorf("table output:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "\x1b[") {
		t.Error("color codes without Color")
	}
}

// Every column must start at the same offset on every line, with or without
// colors.
func TestTableAlignment(t *testing.T) {
	for _, format := range []string{"table", "wide"} {
		plain := render(t, sampleReport(), RenderOptions{Format: format, ShowNamespace: true})
		colored := render(t, sampleReport(), RenderOptions{Format: format, ShowNamespace: true, Color: true})
		if stripANSI(colored) != plain {
			t.Errorf("%s: colors change the layout", format)
		}
		if !strings.Contains(colored, ansiRed+"idle 9 days, reclaim"+ansiReset) || !strings.Contains(colored, ansiGreen+"keep as is"+ansiReset) ||
			!strings.Contains(colored, ansiYellow+"MIG 1g.10gb slice"+ansiReset) || !strings.Contains(colored, ansiBold+"WORKLOAD"+ansiReset) {
			t.Errorf("%s: expected colors missing:\n%q", format, colored)
		}

		lines := strings.Split(plain, "\n")
		header := lines[0]
		var cols []string
		switch format {
		case "table":
			cols = []string{"NAMESPACE", "WORKLOAD", "REQUESTED", "PEAK MEM", "AVG UTIL", "RECOMMENDATION", "WASTE/MO"}
		default:
			cols = []string{"NAMESPACE", "WORKLOAD", "KIND", "REQUESTED", "GPU MODEL", "PEAK MEM", "AVG UTIL", "P95 UTIL", "MAX UTIL", "OBSERVED", "RECOMMENDATION", "COST/MO", "WASTE/MO", "REASON"}
		}
		if !strings.HasPrefix(header, cols[0]) {
			t.Fatalf("%s header = %q", format, header)
		}
		for _, col := range cols[1:] {
			off := strings.Index(header, col)
			for _, line := range lines[1:5] {
				if utf8.RuneCountInString(line) <= off {
					t.Fatalf("%s: line too short for column %s: %q", format, col, line)
				}
				if line[off] == ' ' || line[off-1] != ' ' || line[off-2] != ' ' {
					t.Errorf("%s: column %s misaligned in %q", format, col, line)
				}
			}
		}
		for _, line := range lines {
			if strings.TrimRight(line, " ") != line {
				t.Errorf("%s: trailing whitespace in %q", format, line)
			}
		}
	}
}

func stripANSI(s string) string {
	for _, code := range []string{ansiReset, ansiBold, ansiDim, ansiRed, ansiGreen, ansiYellow} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}

func TestRenderWideAndExplain(t *testing.T) {
	wide := render(t, sampleReport(), RenderOptions{Format: "wide"})
	for _, want := range []string{"Deployment", "A100 80GB", "12%", "$11,000", "Fits | the slice.", "7 days", "ml-serving"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide output lacks %q:\n%s", want, wide)
		}
	}
	explained := render(t, sampleReport(), RenderOptions{Format: "table", Explain: true})
	for _, want := range []string{
		"ml-serving/embed-api (Deployment)\n",
		"  recommendation: MIG 1g.10gb slice\n",
		"  why:            Fits | the slice.\n",
		"  alternatives:   time-slice, 4 per GPU\n",
		"  cost:           $11,000/mo at $3.67 per GPU-hour, est. waste $9,200/mo\n",
		"sandbox/mystery (Pod)\n",
	} {
		if !strings.Contains(explained, want) {
			t.Errorf("explain output lacks %q:\n%s", want, explained)
		}
	}
}

func TestRenderEmptyAndFiltered(t *testing.T) {
	rep := &Report{Window: "7d", Headroom: 0.2, Namespace: "default", PriceNote: PriceNote}
	got := render(t, rep, RenderOptions{})
	if !strings.Contains(got, `No running pods requesting NVIDIA GPUs found in namespace "default" (use -A for all namespaces).`) ||
		!strings.Contains(got, "0 workloads, 0 GPUs, est. cost $0/mo, est. waste $0/mo\n") {
		t.Errorf("empty output:\n%s", got)
	}
	rep.Namespace = ""
	if got := render(t, rep, RenderOptions{}); !strings.Contains(got, "found in any namespace.") {
		t.Errorf("empty output:\n%s", got)
	}

	rep = sampleReport()
	rep.Rows = rep.Rows[:1]
	rep.Summary.Shown = 1
	rep.Warnings = []string{"could not list nodes"}
	got = render(t, rep, RenderOptions{})
	if !strings.Contains(got, "Showing 1 of 4 workloads (filtered by --min-waste).") || !strings.Contains(got, "warning: could not list nodes\n") {
		t.Errorf("filtered output:\n%s", got)
	}
	rep.Rows, rep.Summary.Shown = nil, 0
	if got := render(t, rep, RenderOptions{}); !strings.Contains(got, "No workloads with at least the requested waste.") {
		t.Errorf("output:\n%s", got)
	}

	one := sampleReport()
	one.Rows = one.Rows[2:3]
	one.Summary = Summarize(one.Rows)
	if got := render(t, one, RenderOptions{}); !strings.Contains(got, "1 workload, 1 GPU, est. cost $2,700/mo, est. waste $2,700/mo (100%)") {
		t.Errorf("singular footer:\n%s", got)
	}
}

func TestRenderMarkdown(t *testing.T) {
	got := render(t, sampleReport(), RenderOptions{Format: "markdown", ShowNamespace: true, Explain: true})
	want := `| Namespace | Workload | Requested | Peak mem | Avg util | Recommendation | Waste/mo | Why |
| --- | --- | --- | ---: | ---: | --- | ---: | --- |
| ml-serving | embed-api (x4) | 1x A100 | 6.1 GB | 11% | MIG 1g.10gb slice | $9,200 | Fits \| the slice. |
| ml-training | train-nightly | 4x H100 | 71 GB | 88% | keep as is | - | Busy. |
| notebooks | notebook-priya | 1x A100 | 0.3 GB | 0% | idle 9 days, reclaim | $2,700 | Idle. |
| sandbox | mystery | 1x GPU | - | - | no data | - | No metrics. |

**4 workloads, 10 GPUs, est. cost $42,000/mo, est. waste $12,000/mo (28%)**

_Window 7d, memory headroom 20%, all namespaces. ` + PriceNote + "_\n"
	if got != want {
		t.Errorf("markdown output:\n%s\nwant:\n%s", got, want)
	}
	short := render(t, sampleReport(), RenderOptions{Format: "md"})
	if !strings.HasPrefix(short, "| Workload | Requested |") {
		t.Errorf("md alias output:\n%s", short)
	}
}

func TestControlCharactersAreStripped(t *testing.T) {
	rep := sampleReport()
	rep.Rows = rep.Rows[3:]
	rep.Rows[0].Requested.ModelName = "Evil\x1b[31mGPU\nsecond line"
	rep.Rows[0].Workload = "my\x07stery"
	rep.Rows[0].Recommendation.Reason = "No\rmetrics."
	for _, opts := range []RenderOptions{{Format: "wide"}, {Format: "table", Explain: true}, {Format: "markdown", Explain: true}} {
		out := render(t, rep, opts)
		if strings.ContainsAny(out, "\x1b\x07\r") || strings.Contains(out, "\nsecond line") {
			t.Errorf("%s: control characters leaked: %q", opts.Format, out)
		}
		if !strings.Contains(out, "mystery") || !strings.Contains(out, "Nometrics.") {
			t.Errorf("%s: text mangled: %q", opts.Format, out)
		}
	}
	if out := render(t, rep, RenderOptions{Format: "wide"}); !strings.Contains(out, "Evil[31mGPUsecond line") {
		t.Errorf("wide: %q", out)
	}
}

func TestRenderJSONAndYAML(t *testing.T) {
	rep := sampleReport()
	var fromJSON, fromYAML Report
	if err := json.Unmarshal([]byte(render(t, rep, RenderOptions{Format: "json"})), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(render(t, rep, RenderOptions{Format: "yaml"})), &fromYAML); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]Report{"json": fromJSON, "yaml": fromYAML} {
		if len(got.Rows) != 4 || got.Summary.MonthlyWaste != rep.Summary.MonthlyWaste || got.Window != "7d" || got.PriceNote == "" {
			t.Errorf("%s round trip: %+v", name, got.Summary)
		}
		r := got.Rows[0]
		if r.Recommendation.Action != recommend.ActionMIG || *r.PeakMemMiB != 6246 || r.Requested.Model != "a100-80gb" || r.MonthlyWaste != 9185.5 {
			t.Errorf("%s row: %+v", name, r)
		}
		if got.Rows[3].PeakMemMiB != nil || got.Rows[3].HourlyPrice != nil {
			t.Errorf("%s: missing metrics must stay null", name)
		}
	}
	raw := render(t, rep, RenderOptions{Format: "json"})
	for _, key := range []string{`"peakMemMiB": null`, `"monthlyWasteUSD": 9185.5`, `"avgUtilPercent": 11`, `"keepFraction"`, `"priceNote"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("JSON lacks %s", key)
		}
	}
	if err := Render(&bytes.Buffer{}, rep, RenderOptions{Format: "xml"}); err == nil {
		t.Error("unknown format should fail")
	}
}
