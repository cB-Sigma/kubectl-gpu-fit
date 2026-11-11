// Package cmd implements the kubectl-gpu-fit command line.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/dcgm"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/demo"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kube"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/pricing"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/prom"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/recommend"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/report"
)

// Build information, set with -ldflags at release time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// EnvPrometheusURL is the environment variable read when --prometheus-url
// is not given.
const EnvPrometheusURL = "GPU_FIT_PROMETHEUS_URL"

const maxDiscoveryAttempts = 5

// Options holds the parsed command line.
type Options struct {
	configFlags   *genericclioptions.ConfigFlags
	allNamespaces bool

	prometheusURL       string
	prometheusService   string
	prometheusTokenFile string
	timeout             time.Duration

	window         string
	resolution     string
	headroom       string
	idleAfter      string
	idleUtil       float64
	minObservation string
	strategies     []string
	maxTimeSlice   int

	prices    []string
	priceFile string

	output   string
	sortBy   string
	minWaste float64
	explain  bool
	color    string
	demo     bool

	// Out and ErrOut are the output streams.
	Out    io.Writer
	ErrOut io.Writer
	// NewClient builds the Kubernetes client and returns the namespace
	// selected by kubeconfig and flags. Tests replace it.
	NewClient func() (kubernetes.Interface, string, error)
	// Now returns the evaluation time. Tests replace it.
	Now func() time.Time
}

const longDescription = `kubectl gpu-fit is a read-only GPU right-sizer for Kubernetes.

It reads the NVIDIA DCGM exporter metrics your cluster already stores in
Prometheus, joins them with the GPU requests of running pods, and reports
for every workload what it actually needs: keep the GPU, move to a MIG
slice, share via time-slicing, use a smaller GPU model, or reclaim an idle
GPU. It also estimates the monthly cost of what is not needed.

Nothing in the cluster is ever modified.`

const examples = `  # Workloads in the current namespace, last 7 days
  kubectl gpu-fit

  # Whole cluster, last 14 days, biggest waste first, with reasoning
  kubectl gpu-fit -A --window 14d --explain

  # Point at a Prometheus you can reach directly
  kubectl gpu-fit -A --prometheus-url http://localhost:9090

  # Use a specific in-cluster Prometheus through the API server proxy
  kubectl gpu-fit -A --prometheus-service monitoring/prometheus-k8s:9090

  # Your own prices, machine-readable output
  kubectl gpu-fit -A --price a100=3.67 --price-file prices.yaml -o json

  # No cluster at hand? See what the report looks like
  kubectl gpu-fit --demo`

// NewRootCommand builds the root command.
func NewRootCommand(out, errOut io.Writer) *cobra.Command {
	o := &Options{
		configFlags: genericclioptions.NewConfigFlags(true),
		Out:         out,
		ErrOut:      errOut,
		Now:         time.Now,
	}
	o.NewClient = o.clientFromFlags
	return newCommand(o)
}

func newCommand(o *Options) *cobra.Command {
	defaults := recommend.DefaultOptions()
	cmd := &cobra.Command{
		Use:           "kubectl-gpu-fit",
		Short:         "Right-size GPU requests from the DCGM metrics you already have",
		Long:          longDescription,
		Example:       examples,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		Annotations: map[string]string{
			// Makes help read "kubectl gpu-fit" when run as a plugin.
			cobra.CommandDisplayNameAnnotation: "kubectl gpu-fit",
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return o.Run(c.Context())
		},
	}
	cmd.SetOut(o.Out)
	cmd.SetErr(o.ErrOut)
	cmd.SetVersionTemplate(fmt.Sprintf("kubectl-gpu-fit %s (commit %s, built %s)\n", Version, Commit, Date))

	f := cmd.Flags()
	o.configFlags.AddFlags(f)
	f.BoolVarP(&o.allNamespaces, "all-namespaces", "A", false, "Analyse workloads in all namespaces")

	f.StringVar(&o.prometheusURL, "prometheus-url", "", "Base URL of a Prometheus-compatible server (env "+EnvPrometheusURL+"). When empty, an in-cluster Prometheus Service is discovered and queried through the API server proxy")
	f.StringVar(&o.prometheusService, "prometheus-service", "", "In-cluster Prometheus Service to query through the API server proxy, as [https:]namespace/name[:port]. Skips discovery")
	f.StringVar(&o.prometheusTokenFile, "prometheus-token-file", "", "File with a bearer token to send to --prometheus-url")
	f.DurationVar(&o.timeout, "timeout", 2*time.Minute, "Overall time limit for cluster and Prometheus queries")

	f.StringVar(&o.window, "window", "7d", "How far back to look at usage (e.g. 24h, 7d, 2w)")
	f.StringVar(&o.resolution, "resolution", "5m", "Step used to measure coverage and idle time")
	f.StringVar(&o.headroom, "headroom", "20%", "Safety margin added to peak memory before anything smaller is suggested (20% or 0.2)")
	f.StringVar(&o.idleAfter, "idle-after", "24h", "Report a GPU as idle once it has been unused for this long")
	f.Float64Var(&o.idleUtil, "idle-util", defaults.IdleUtil, "GPU utilization (percent) at or below which a GPU counts as unused")
	f.StringVar(&o.minObservation, "min-observation", "1h", "Minimum metric history a workload needs before it gets a recommendation")
	f.StringSliceVar(&o.strategies, "strategies", recommend.AllStrategies, "Right-sizing strategies to consider, in order of preference")
	f.IntVar(&o.maxTimeSlice, "max-time-slice", defaults.MaxTimeSlice, "Largest number of workloads to suggest packing on one time-sliced GPU")

	// Price flags are shared with the "models" subcommand.
	pf := cmd.PersistentFlags()
	pf.StringArrayVar(&o.prices, "price", nil, "Override an hourly GPU price in USD, as model=price (e.g. a100=3.67, h100=4.25). Repeatable")
	pf.StringVar(&o.priceFile, "price-file", "", "YAML file with hourly GPU prices (see examples/prices.yaml)")

	f.StringVarP(&o.output, "output", "o", "table", "Output format: "+strings.Join(report.Formats, ", "))
	f.StringVar(&o.sortBy, "sort-by", "waste", "Sort rows by: "+strings.Join(report.SortKeys, ", "))
	f.Float64Var(&o.minWaste, "min-waste", 0, "Only show workloads with at least this much estimated monthly waste (USD)")
	f.BoolVar(&o.explain, "explain", false, "Print the reasoning behind every recommendation")
	f.StringVar(&o.color, "color", "auto", "Colorize table output: auto, always, never (auto honours NO_COLOR)")
	f.BoolVar(&o.demo, "demo", false, "Show a report built from bundled sample data; needs no cluster")

	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(c *cobra.Command, _ []string) {
			fmt.Fprintf(c.OutOrStdout(), "kubectl-gpu-fit %s (commit %s, built %s)\n", Version, Commit, Date)
		},
	})
	cmd.AddCommand(newModelsCommand(o))
	return cmd
}

func (o *Options) clientFromFlags() (kubernetes.Interface, string, error) {
	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return nil, "", fmt.Errorf("loading kubeconfig: %w", err)
	}
	cfg.UserAgent = "kubectl-gpu-fit/" + Version
	// The report issues a burst of small read requests; the client-side
	// defaults (5 QPS) would only slow it down.
	cfg.QPS, cfg.Burst = 50, 100
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("creating Kubernetes client: %w", err)
	}
	ns, _, err := o.configFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("determining namespace: %w", err)
	}
	return client, ns, nil
}

type settings struct {
	window, resolution time.Duration
	engine             recommend.Options
	prices             *pricing.Table
	customPrices       bool
}

func (o *Options) parse() (*settings, error) {
	s := &settings{engine: recommend.DefaultOptions()}
	var err error
	if s.window, err = parseDuration(o.window); err != nil {
		return nil, fmt.Errorf("--window: %w", err)
	}
	if s.resolution, err = parseDuration(o.resolution); err != nil {
		return nil, fmt.Errorf("--resolution: %w", err)
	}
	if s.resolution > s.window {
		return nil, fmt.Errorf("--resolution (%s) must not exceed --window (%s)", o.resolution, o.window)
	}
	// Subqueries are evaluated at every step; keep the point count sane.
	if s.window/s.resolution > 11000 {
		return nil, fmt.Errorf("--window %s at --resolution %s needs more than 11000 points per series; use a coarser resolution", o.window, o.resolution)
	}
	if s.engine.Headroom, err = parseFraction(o.headroom); err != nil {
		return nil, fmt.Errorf("--headroom: %w", err)
	}
	if s.engine.IdleAfter, err = parseDuration(o.idleAfter); err != nil {
		return nil, fmt.Errorf("--idle-after: %w", err)
	}
	if s.engine.MinObservation, err = parseDuration(o.minObservation); err != nil {
		return nil, fmt.Errorf("--min-observation: %w", err)
	}
	if s.engine.IdleAfter > s.window {
		return nil, fmt.Errorf("--idle-after (%s) must not exceed --window (%s): idleness cannot be established beyond the window", o.idleAfter, o.window)
	}
	s.engine.IdleUtil = o.idleUtil
	s.engine.MaxTimeSlice = o.maxTimeSlice
	s.engine.Strategies = []string{}
	for _, st := range o.strategies {
		if st = strings.ToLower(strings.TrimSpace(st)); st != "" && st != "none" {
			s.engine.Strategies = append(s.engine.Strategies, st)
		}
	}
	if err := s.engine.Validate(); err != nil {
		return nil, err
	}

	valid := false
	for _, f := range append([]string{"md"}, report.Formats...) {
		valid = valid || f == o.output
	}
	if !valid {
		return nil, fmt.Errorf("unknown output format %q (known: %s)", o.output, strings.Join(report.Formats, ", "))
	}
	if err := report.Sort(nil, o.sortBy); err != nil {
		return nil, err
	}
	if o.minWaste < 0 {
		return nil, fmt.Errorf("--min-waste must not be negative")
	}
	switch o.color {
	case "auto", "always", "never":
	default:
		return nil, fmt.Errorf("--color must be auto, always or never, got %q", o.color)
	}

	s.prices = pricing.Defaults()
	if o.priceFile != "" {
		if err := s.prices.LoadFile(o.priceFile); err != nil {
			return nil, err
		}
	}
	if err := s.prices.ApplyOverrides(o.prices); err != nil {
		return nil, err
	}
	s.customPrices = o.priceFile != "" || len(o.prices) > 0
	return s, nil
}

// Run executes the report.
func (o *Options) Run(ctx context.Context) error {
	s, err := o.parse()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	now := o.Now()
	rep := &report.Report{
		GeneratedAt: now.UTC().Truncate(time.Second),
		Window:      formatWindow(s.window),
		Headroom:    s.engine.Headroom,
		PriceNote:   report.PriceNote,
	}
	if s.customPrices {
		rep.PriceNote = report.CustomPriceNote
	}
	in := report.Input{Prices: s.prices, Options: s.engine, Now: now}

	if o.demo {
		in.Workloads, in.Usage = demo.Data(now, s.window)
		rep.Source = demo.Source
	} else {
		if err := o.loadCluster(ctx, s, rep, &in); err != nil {
			return err
		}
	}

	rows := report.Build(in)
	rep.Summary = report.Summarize(rows)
	rows = report.FilterMinWaste(rows, o.minWaste)
	rep.Summary.Shown = len(rows)
	if err := report.Sort(rows, o.sortBy); err != nil {
		return err
	}
	rep.Rows = rows

	return report.Render(o.Out, rep, report.RenderOptions{
		Format:        o.output,
		Color:         o.useColor(),
		Explain:       o.explain,
		ShowNamespace: rep.Namespace == "",
	})
}

func (o *Options) loadCluster(ctx context.Context, s *settings, rep *report.Report, in *report.Input) error {
	client, namespace, err := o.NewClient()
	if err != nil {
		return err
	}
	if o.allNamespaces {
		namespace = ""
	}
	rep.Namespace = namespace

	snap, err := kube.Load(ctx, client, namespace)
	if err != nil {
		return err
	}
	in.Workloads, in.Nodes = snap.Workloads, snap.Nodes
	rep.Warnings = append(rep.Warnings, snap.Warnings...)
	if len(snap.Workloads) == 0 {
		// Nothing to analyse: do not bother Prometheus.
		rep.Source = "not queried (no GPU workloads)"
		return nil
	}

	pc, err := o.prometheusClient(ctx, client)
	if err != nil {
		return err
	}
	rep.Source = "Prometheus at " + pc.Endpoint()

	usage, stats, err := dcgm.Collect(ctx, pc, dcgm.Options{
		Window:    s.window,
		Recent:    s.engine.IdleAfter,
		Step:      s.resolution,
		IdleUtil:  s.engine.IdleUtil,
		Namespace: namespace,
		Now:       in.Now,
	})
	if err != nil {
		return fmt.Errorf("reading DCGM metrics from %s: %w", pc.Endpoint(), err)
	}
	in.Usage = usage
	switch {
	case stats.Series == 0:
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"%s has no DCGM metrics for the last %s (looked for DCGM_FI_DEV_GPU_UTIL[_RATIO] and DCGM_FI_DEV_FB_USED[_BYTES]). Is dcgm-exporter installed and scraped by this Prometheus?",
			rep.Source, rep.Window))
	case stats.Unattributed == stats.Series:
		rep.Warnings = append(rep.Warnings,
			"DCGM metrics were found but none carry pod labels. dcgm-exporter must run with Kubernetes pod attribution enabled (the GPU Operator does this by default; standalone: DCGM_EXPORTER_KUBERNETES=true).")
	}
	return nil
}

func (o *Options) prometheusClient(ctx context.Context, client kubernetes.Interface) (*prom.Client, error) {
	promURL := o.prometheusURL
	if promURL == "" && o.prometheusService == "" {
		promURL = os.Getenv(EnvPrometheusURL)
	}
	if promURL != "" {
		token := ""
		if o.prometheusTokenFile != "" {
			data, err := os.ReadFile(o.prometheusTokenFile)
			if err != nil {
				return nil, fmt.Errorf("reading --prometheus-token-file: %w", err)
			}
			token = string(data)
		}
		t, err := prom.NewHTTPTransport(promURL, token, o.timeout)
		if err != nil {
			return nil, err
		}
		return usable(ctx, prom.NewClient(t))
	}

	if o.prometheusService != "" {
		ref, err := prom.ParseServiceRef(o.prometheusService)
		if err != nil {
			return nil, fmt.Errorf("--prometheus-service: %w", err)
		}
		return usable(ctx, prom.NewClient(prom.NewProxyTransport(client, ref)))
	}

	refs, err := prom.Discover(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("%w; pass --prometheus-url or --prometheus-service", err)
	}
	if len(refs) == 0 {
		return nil, errors.New("no Prometheus Service found in the cluster; pass --prometheus-url or --prometheus-service namespace/name:port")
	}
	if len(refs) > maxDiscoveryAttempts {
		refs = refs[:maxDiscoveryAttempts]
	}
	var failures []string
	for _, ref := range refs {
		pc := prom.NewClient(prom.NewProxyTransport(client, ref))
		if err := pc.Ping(ctx); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", ref, err))
			continue
		}
		return pc, nil
	}
	return nil, fmt.Errorf("found %d Prometheus Service candidate(s) but none answered through the API server proxy; pass --prometheus-url or --prometheus-service\n  %s",
		len(refs), strings.Join(failures, "\n  "))
}

// usable probes an explicitly configured Prometheus so that a wrong URL or
// Service fails with one clear message instead of a failed PromQL query.
func usable(ctx context.Context, pc *prom.Client) (*prom.Client, error) {
	if err := pc.Ping(ctx); err != nil {
		return nil, fmt.Errorf("cannot query Prometheus at %s: %w", pc.Endpoint(), err)
	}
	return pc, nil
}

func (o *Options) useColor() bool {
	switch o.color {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := o.Out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
