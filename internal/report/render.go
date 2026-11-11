package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/recommend"
)

// Formats lists the accepted --output values.
var Formats = []string{"table", "wide", "json", "yaml", "markdown"}

// RenderOptions control rendering.
type RenderOptions struct {
	// Format is one of Formats.
	Format string
	// Color enables ANSI colors in table and wide output.
	Color bool
	// Explain appends the reasoning for every row to table output.
	Explain bool
	// ShowNamespace adds a NAMESPACE column to the table.
	ShowNamespace bool
}

// Render writes the report in the requested format.
func Render(w io.Writer, rep *Report, opts RenderOptions) error {
	switch opts.Format {
	case "", "table":
		return renderTable(w, rep, opts, false)
	case "wide":
		return renderTable(w, rep, opts, true)
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "yaml":
		out, err := yaml.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = w.Write(out)
		return err
	case "markdown", "md":
		return renderMarkdown(w, rep, opts)
	default:
		return fmt.Errorf("unknown output format %q (known: %s)", opts.Format, strings.Join(Formats, ", "))
	}
}

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

func actionColor(a recommend.Action) string {
	switch a {
	case recommend.ActionReclaim:
		return ansiRed
	case recommend.ActionMIG, recommend.ActionTimeSlice, recommend.ActionDownsize, recommend.ActionReview:
		return ansiYellow
	case recommend.ActionKeep:
		return ansiGreen
	default:
		return ansiDim
	}
}

type cell struct {
	text  string
	color string
}

func workloadName(r Row) string {
	if r.Replicas > 1 {
		return fmt.Sprintf("%s (x%d)", r.Workload, r.Replicas)
	}
	return r.Workload
}

func fmtMem(p *float64) string {
	if p == nil {
		return "-"
	}
	return recommend.FormatMiB(*p)
}

func fmtPct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// FormatMoney renders a dollar amount rounded to two significant digits
// (nearest $10 below $1,000): the inputs are estimates, and the output
// should not suggest otherwise.
func FormatMoney(v float64) string {
	if v < 0 {
		v = 0
	}
	var rounded float64
	switch {
	case v < 1000:
		rounded = math.Round(v/10) * 10
	default:
		mag := math.Pow(10, math.Floor(math.Log10(v))-1)
		rounded = math.Round(v/mag) * mag
	}
	s := strconv.FormatInt(int64(rounded), 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return "$" + b.String()
}

func fmtWaste(r Row) string {
	switch {
	case r.MonthlyWaste >= 5:
		return FormatMoney(r.MonthlyWaste)
	case r.Recommendation.KeepFraction < 1 && r.MonthlyCost == 0:
		return "?" // savings exist but cannot be priced
	default:
		return "-"
	}
}

func fmtGPUs(g float64) string {
	if math.Abs(g-math.Round(g)) < 0.05 {
		return strconv.Itoa(int(math.Round(g)))
	}
	return fmt.Sprintf("%.1f", g)
}

func fmtObserved(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	return recommend.FormatDuration(time.Duration(seconds * float64(time.Second)))
}

func footer(rep *Report) []string {
	s := rep.Summary
	line := fmt.Sprintf("%d %s, %s %s, est. cost %s/mo, est. waste %s/mo",
		s.Workloads, plural(s.Workloads, "workload"), fmtGPUs(s.GPUs), pluralF(s.GPUs, "GPU"),
		FormatMoney(s.MonthlyCost), FormatMoney(s.MonthlyWaste))
	if s.MonthlyCost > 0 {
		line += fmt.Sprintf(" (%.0f%%)", 100*s.MonthlyWaste/s.MonthlyCost)
	}
	ns := "all namespaces"
	if rep.Namespace != "" {
		ns = "namespace " + rep.Namespace
	}
	lines := []string{line}
	if s.Shown < s.Workloads {
		lines = append(lines, fmt.Sprintf("Showing %d of %d workloads (filtered by --min-waste).", s.Shown, s.Workloads))
	}
	lines = append(lines,
		fmt.Sprintf("Window %s, memory headroom %.0f%%, %s. %s", rep.Window, rep.Headroom*100, ns, rep.PriceNote))
	return lines
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func pluralF(n float64, word string) string {
	if math.Abs(n-1) < 0.05 {
		return word
	}
	return word + "s"
}

func renderTable(w io.Writer, rep *Report, opts RenderOptions, wide bool) error {
	var b strings.Builder
	if len(rep.Rows) == 0 {
		where := "in any namespace"
		if rep.Namespace != "" {
			where = fmt.Sprintf("in namespace %q (use -A for all namespaces)", rep.Namespace)
		}
		if rep.Summary.Workloads > 0 {
			fmt.Fprintf(&b, "No workloads with at least the requested waste.\n")
		} else {
			fmt.Fprintf(&b, "No running pods requesting NVIDIA GPUs found %s.\n", where)
		}
	} else {
		showNS := opts.ShowNamespace || wide
		var header []string
		if showNS {
			header = append(header, "NAMESPACE")
		}
		header = append(header, "WORKLOAD")
		if wide {
			header = append(header, "KIND")
		}
		header = append(header, "REQUESTED")
		if wide {
			header = append(header, "GPU MODEL")
		}
		header = append(header, "PEAK MEM", "AVG UTIL")
		if wide {
			header = append(header, "P95 UTIL", "MAX UTIL", "OBSERVED")
		}
		header = append(header, "RECOMMENDATION")
		if wide {
			header = append(header, "COST/MO")
		}
		header = append(header, "WASTE/MO")
		if wide {
			header = append(header, "REASON")
		}

		table := make([][]cell, 0, len(rep.Rows)+1)
		head := make([]cell, len(header))
		for i, h := range header {
			head[i] = cell{h, ansiBold}
		}
		table = append(table, head)
		for _, r := range rep.Rows {
			color := actionColor(r.Recommendation.Action)
			var line []cell
			if showNS {
				line = append(line, cell{text: r.Namespace})
			}
			line = append(line, cell{text: workloadName(r)})
			if wide {
				line = append(line, cell{text: r.Kind})
			}
			line = append(line, cell{text: r.Requested.Text})
			if wide {
				line = append(line, cell{text: orDash(r.Requested.ModelName)})
			}
			line = append(line, cell{text: fmtMem(r.PeakMemMiB)}, cell{text: fmtPct(r.AvgUtil)})
			if wide {
				line = append(line, cell{text: fmtPct(r.P95Util)}, cell{text: fmtPct(r.MaxUtil)}, cell{text: fmtObserved(r.ObservedSeconds)})
			}
			line = append(line, cell{r.Recommendation.Summary, color})
			if wide {
				cost := "-"
				if r.MonthlyCost > 0 {
					cost = FormatMoney(r.MonthlyCost)
				}
				line = append(line, cell{text: cost})
			}
			wasteColor := ""
			if r.MonthlyWaste >= 5 {
				wasteColor = color
			}
			line = append(line, cell{fmtWaste(r), wasteColor})
			if wide {
				line = append(line, cell{text: r.Recommendation.Reason})
			}
			table = append(table, line)
		}
		writeAligned(&b, table, opts.Color)
	}

	if opts.Explain && len(rep.Rows) > 0 {
		for _, r := range rep.Rows {
			b.WriteString("\n")
			title := sanitize(fmt.Sprintf("%s/%s (%s)", r.Namespace, r.Workload, r.Kind))
			if opts.Color {
				title = ansiBold + title + ansiReset
			}
			fmt.Fprintf(&b, "%s\n", title)
			fmt.Fprintf(&b, "  recommendation: %s\n", r.Recommendation.Summary)
			fmt.Fprintf(&b, "  why:            %s\n", sanitize(r.Recommendation.Reason))
			if len(r.Recommendation.Alternatives) > 0 {
				fmt.Fprintf(&b, "  alternatives:   %s\n", strings.Join(r.Recommendation.Alternatives, "; "))
			}
			if r.HourlyPrice != nil && r.MonthlyCost > 0 {
				fmt.Fprintf(&b, "  cost:           %s/mo at $%.2f per GPU-hour, est. waste %s/mo\n",
					FormatMoney(r.MonthlyCost), *r.HourlyPrice, FormatMoney(r.MonthlyWaste))
			}
		}
	}

	b.WriteString("\n")
	for i, line := range footer(rep) {
		if opts.Color && i > 0 {
			line = ansiDim + line + ansiReset
		}
		b.WriteString(line + "\n")
	}
	for _, warn := range rep.Warnings {
		line := "warning: " + warn
		if opts.Color {
			line = ansiYellow + line + ansiReset
		}
		b.WriteString(line + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// sanitize drops control characters. Names and model strings come from the
// cluster and from metric labels; they must not be able to smuggle terminal
// escape sequences or line breaks into the output.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// writeAligned pads every column to its widest cell. Padding is computed on
// the plain text so color codes never disturb the alignment, and the last
// column is not padded.
func writeAligned(b *strings.Builder, table [][]cell, color bool) {
	widths := make([]int, len(table[0]))
	for _, line := range table {
		for i := range line {
			line[i].text = sanitize(line[i].text)
		}
		for i, c := range line {
			if n := utf8.RuneCountInString(c.text); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for _, line := range table {
		for i, c := range line {
			text := c.text
			if color && c.color != "" {
				text = c.color + text + ansiReset
			}
			b.WriteString(text)
			if i < len(line)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text)+3))
			}
		}
		b.WriteString("\n")
	}
}

func renderMarkdown(w io.Writer, rep *Report, opts RenderOptions) error {
	var b strings.Builder
	header := []string{"Workload", "Requested", "Peak mem", "Avg util", "Recommendation", "Waste/mo"}
	if opts.ShowNamespace {
		header = append([]string{"Namespace"}, header...)
	}
	if opts.Explain {
		header = append(header, "Why")
	}
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	seps := make([]string, len(header))
	for i, h := range header {
		seps[i] = "---"
		if h == "Peak mem" || h == "Avg util" || h == "Waste/mo" {
			seps[i] = "---:"
		}
	}
	b.WriteString("| " + strings.Join(seps, " | ") + " |\n")
	for _, r := range rep.Rows {
		cols := []string{workloadName(r), r.Requested.Text, fmtMem(r.PeakMemMiB), fmtPct(r.AvgUtil), r.Recommendation.Summary, fmtWaste(r)}
		if opts.ShowNamespace {
			cols = append([]string{r.Namespace}, cols...)
		}
		if opts.Explain {
			cols = append(cols, r.Recommendation.Reason)
		}
		for i := range cols {
			cols[i] = strings.ReplaceAll(sanitize(cols[i]), "|", "\\|")
		}
		b.WriteString("| " + strings.Join(cols, " | ") + " |\n")
	}
	b.WriteString("\n")
	lines := footer(rep)
	b.WriteString("**" + lines[0] + "**\n\n")
	for _, line := range lines[1:] {
		b.WriteString("_" + line + "_\n\n")
	}
	for _, warn := range rep.Warnings {
		b.WriteString("> warning: " + warn + "\n")
	}
	_, err := io.WriteString(w, strings.TrimRight(b.String(), "\n")+"\n")
	return err
}
