package cmd

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kubetest"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/promtest"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/report"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/testcluster"
)

var testNow = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// cluster is a fake Kubernetes cluster with a fake Prometheus behind the
// service proxy.
type cluster struct {
	client *fake.Clientset
	prom   *promtest.Server
	proxy  *promtest.Proxy
}

func newCluster(t *testing.T, exportedLabels bool) *cluster {
	t.Helper()
	objs, series := testcluster.New(testNow, exportedLabels)
	c := &cluster{
		client: fake.NewSimpleClientset(objs...),
		prom:   promtest.New("7d", series),
		proxy:  promtest.NewProxy(),
	}
	t.Cleanup(c.prom.Close)
	c.proxy.Register(testcluster.PrometheusNamespace, testcluster.PrometheusService, c.prom.Handler())
	c.client.AddProxyReactor("services", c.proxy.React)
	return c
}

type result struct {
	stdout, stderr string
	err            error
}

func run(t *testing.T, c *cluster, namespace string, args ...string) result {
	t.Helper()
	t.Setenv(EnvPrometheusURL, "")
	var out, errOut bytes.Buffer
	o := &Options{
		configFlags: genericclioptions.NewConfigFlags(true),
		Out:         &out,
		ErrOut:      &errOut,
		Now:         func() time.Time { return testNow },
	}
	o.NewClient = func() (kubernetes.Interface, string, error) {
		if c == nil {
			t.Fatal("command tried to reach a cluster")
		}
		return c.client, namespace, nil
	}
	root := newCommand(o)
	root.SetArgs(args)
	err := root.Execute()
	return result{out.String(), errOut.String(), err}
}

const wantClusterTable = testcluster.WantTable

func TestEndToEndDiscoveryThroughServiceProxy(t *testing.T) {
	for _, exported := range []bool{false, true} {
		c := newCluster(t, exported)
		res := run(t, c, "default", "-A")
		if res.err != nil {
			t.Fatalf("exported=%v: %v", exported, res.err)
		}
		if res.stdout != wantClusterTable {
			t.Errorf("exported=%v: output:\n%s\nwant:\n%s", exported, res.stdout, wantClusterTable)
		}
		if res.stderr != "" {
			t.Errorf("stderr = %q", res.stderr)
		}

		calls := c.proxy.Calls()
		if len(calls) == 0 {
			t.Fatal("Prometheus was not reached through the service proxy")
		}
		for _, call := range calls {
			if call.Namespace != "monitoring" || call.Name != "kube-prometheus-stack-prometheus" || call.Port != "http-web" || call.Path != "api/v1/query" {
				t.Fatalf("unexpected proxy call %+v", call)
			}
		}

		// Strictly read-only against the API server.
		for _, a := range c.client.Actions() {
			if v := a.GetVerb(); v != "get" && v != "list" {
				t.Errorf("non-read request: %s %s", v, a.GetResource().Resource)
			}
		}
	}
}

func TestEndToEndPrometheusURLAndJSON(t *testing.T) {
	c := newCluster(t, false)
	res := run(t, c, "ml-serving", "--prometheus-url", c.prom.URL, "-o", "json", "--price", "a100=3.67")
	if res.err != nil {
		t.Fatal(res.err)
	}
	if len(c.proxy.Calls()) != 0 {
		t.Error("service proxy used although --prometheus-url was given")
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(res.stdout), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, res.stdout)
	}
	if rep.Namespace != "ml-serving" || rep.Window != "7d" || rep.Headroom != 0.2 || !rep.GeneratedAt.Equal(testNow) ||
		rep.Source != "Prometheus at "+c.prom.URL {
		t.Errorf("report header = %+v", rep)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	r := rep.Rows[0]
	if r.Workload != "embed-api" || r.Kind != "Deployment" || r.Replicas != 4 || r.Recommendation.Target != "1g.10gb" {
		t.Errorf("row = %+v", r)
	}
	// 4 GPUs x $3.67 x 730 h x 6/7 saved.
	if want := 4 * 3.67 * 730 * 6 / 7; r.MonthlyWaste < want-0.01 || r.MonthlyWaste > want+0.01 {
		t.Errorf("waste = %v, want %v", r.MonthlyWaste, want)
	}
	if rep.Summary.MonthlyWaste != r.MonthlyWaste || rep.Summary.GPUs != 4 {
		t.Errorf("summary = %+v", rep.Summary)
	}

	// The same through the environment variable.
	t.Run("env", func(t *testing.T) {
		var out bytes.Buffer
		o := &Options{configFlags: genericclioptions.NewConfigFlags(true), Out: &out, ErrOut: &out, Now: func() time.Time { return testNow }}
		o.NewClient = func() (kubernetes.Interface, string, error) { return c.client, "notebooks", nil }
		t.Setenv(EnvPrometheusURL, c.prom.URL)
		root := newCommand(o)
		root.SetArgs([]string{"-o", "markdown"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "| notebook-priya | 1x A100 | 0.3 GB | 0% | idle 5 days, reclaim | $3,700 |") {
			t.Errorf("output:\n%s", out.String())
		}
		if len(c.proxy.Calls()) != 0 {
			t.Error("service proxy used although the env var was set")
		}
	})
}

func TestEndToEndExplicitServiceAndOptions(t *testing.T) {
	c := newCluster(t, false)
	svc := "monitoring/kube-prometheus-stack-prometheus:9090"

	res := run(t, c, "", "-A", "--prometheus-service", svc, "--min-waste", "5000", "--explain")
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, want := range []string{
		"embed-api (x4)", "Showing 1 of 4 workloads (filtered by --min-waste).",
		"ml-serving/embed-api (Deployment)", "fits the 1g.10gb slice of the A100 80GB", "alternatives:   time-slice, 4 per GPU",
		"4 workloads, 10 GPUs",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "notebook-priya") {
		t.Errorf("--min-waste did not filter:\n%s", res.stdout)
	}
	if calls := c.proxy.Calls(); len(calls) == 0 || calls[0].Port != "9090" {
		t.Errorf("proxy calls = %+v", calls)
	}

	// Sorting, a tighter idle rule and disabled strategies.
	res = run(t, c, "", "-A", "--prometheus-service", svc, "--sort-by", "name", "--strategies", "none", "--idle-after", "6d", "-o", "wide")
	if res.err != nil {
		t.Fatal(res.err)
	}
	lines := strings.Split(res.stdout, "\n")
	if !strings.Contains(lines[1], "embed-api") || !strings.Contains(lines[1], "keep as is") || !strings.Contains(lines[1], "Deployment") {
		t.Errorf("embed-api line = %q", lines[1])
	}
	if !strings.Contains(lines[2], "fresh") || !strings.Contains(lines[3], "notebook-priya") || !strings.Contains(lines[3], "keep as is") {
		t.Errorf("unexpected order or idle handling:\n%s", res.stdout)
	}

	// More headroom pushes embed-api (6.1 GB peak) out of the 10 GB slice.
	res = run(t, c, "ml-serving", "--prometheus-service", svc, "--headroom", "60%")
	if res.err != nil || !strings.Contains(res.stdout, "MIG 1g.20gb slice") || !strings.Contains(res.stdout, "memory headroom 60%, namespace ml-serving") {
		t.Errorf("err=%v output:\n%s", res.err, res.stdout)
	}
}

func TestEndToEndProblems(t *testing.T) {
	t.Run("no gpu workloads", func(t *testing.T) {
		c := newCluster(t, false)
		res := run(t, c, "kube-system")
		if res.err != nil || !strings.Contains(res.stdout, `No running pods requesting NVIDIA GPUs found in namespace "kube-system" (use -A for all namespaces).`) {
			t.Errorf("err=%v output:\n%s", res.err, res.stdout)
		}
		if len(c.prom.Queries()) != 0 {
			t.Error("Prometheus queried although there is nothing to analyse")
		}
	})
	t.Run("prometheus without dcgm metrics", func(t *testing.T) {
		c := newCluster(t, false)
		empty := promtest.New("7d", nil)
		defer empty.Close()
		res := run(t, c, "", "-A", "--prometheus-url", empty.URL)
		if res.err != nil {
			t.Fatal(res.err)
		}
		if !strings.Contains(res.stdout, "warning: Prometheus at "+empty.URL+" has no DCGM metrics for the last 7d") ||
			strings.Count(res.stdout, "no data") != 4 || !strings.Contains(res.stdout, "est. waste $0/mo") {
			t.Errorf("output:\n%s", res.stdout)
		}
		// Models still resolve from the node label.
		if !strings.Contains(res.stdout, "4x A100") {
			t.Errorf("node label fallback missing:\n%s", res.stdout)
		}
	})
	t.Run("metrics without pod labels", func(t *testing.T) {
		c := newCluster(t, false)
		bare := promtest.New("7d", []promtest.Series{{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: map[string]string{"gpu": "0", "UUID": "GPU-x"}, Avg: 5}})
		defer bare.Close()
		res := run(t, c, "", "-A", "--prometheus-url", bare.URL)
		if res.err != nil || !strings.Contains(res.stdout, "none carry pod labels") {
			t.Errorf("err=%v output:\n%s", res.err, res.stdout)
		}
	})
	t.Run("no prometheus service", func(t *testing.T) {
		c := newCluster(t, false)
		if err := c.client.CoreV1().Services("monitoring").Delete(t.Context(), "kube-prometheus-stack-prometheus", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		res := run(t, c, "", "-A")
		if res.err == nil || !strings.Contains(res.err.Error(), "no Prometheus Service found") || res.stdout != "" {
			t.Errorf("err=%v stdout=%q", res.err, res.stdout)
		}
	})
	t.Run("discovered service does not answer", func(t *testing.T) {
		c := newCluster(t, false)
		c.proxy.Register("monitoring", "kube-prometheus-stack-prometheus", nil)
		res := run(t, c, "", "-A")
		if res.err == nil || !strings.Contains(res.err.Error(), "none answered through the API server proxy") ||
			!strings.Contains(res.err.Error(), "monitoring/kube-prometheus-stack-prometheus:http-web") {
			t.Errorf("err=%v", res.err)
		}
	})
	t.Run("dcgm query failure", func(t *testing.T) {
		c := newCluster(t, false)
		c.prom.FailQueries(regexp.MustCompile("FB_USED"))
		res := run(t, c, "", "-A")
		if res.err == nil || !strings.Contains(res.err.Error(), "reading DCGM metrics from service monitoring/kube-prometheus-stack-prometheus:http-web") ||
			!strings.Contains(res.err.Error(), "query failed on purpose") {
			t.Errorf("err=%v", res.err)
		}
	})
	t.Run("unreachable prometheus", func(t *testing.T) {
		c := newCluster(t, false)
		res := run(t, c, "", "-A", "--prometheus-url", "http://127.0.0.1:1")
		if res.err == nil || !strings.Contains(res.err.Error(), "cannot query Prometheus at http://127.0.0.1:1") {
			t.Errorf("err=%v", res.err)
		}
	})
}

func TestFlagValidation(t *testing.T) {
	dir := t.TempDir()
	badPrices := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPrices, []byte("prices:\n  warp9: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--window", "soon"}, "--window: invalid duration"},
		{[]string{"--window", "0h"}, "must be positive"},
		{[]string{"--resolution", "30d"}, "must not exceed --window"},
		{[]string{"--window", "90d", "--resolution", "10s"}, "coarser resolution"},
		{[]string{"--headroom", "20"}, "ambiguous value"},
		{[]string{"--headroom", "lots"}, "--headroom: invalid fraction"},
		{[]string{"--idle-after", "30d"}, "--idle-after (30d) must not exceed --window (7d)"},
		{[]string{"--idle-util", "100"}, "idle utilization threshold"},
		{[]string{"--strategies", "mig,teleport"}, `unknown strategy "teleport"`},
		{[]string{"--max-time-slice", "1"}, "at least 2"},
		{[]string{"-o", "xml"}, `unknown output format "xml"`},
		{[]string{"--sort-by", "vibes"}, `unknown sort key "vibes"`},
		{[]string{"--min-waste", "-1"}, "must not be negative"},
		{[]string{"--color", "sometimes"}, "--color must be"},
		{[]string{"--price", "a100"}, "want model=usd-per-hour"},
		{[]string{"--price", "tesla-k80=1"}, `unknown GPU model "tesla-k80"`},
		{[]string{"--price-file", badPrices}, `unknown GPU model "warp9"`},
		{[]string{"--price-file", filepath.Join(dir, "nope.yaml")}, "reading price file"},
		{[]string{"extra-arg"}, "unknown command"},
	}
	for _, tt := range tests {
		// --demo guarantees that validation fails before any cluster access.
		res := run(t, nil, "", append([]string{"--demo"}, tt.args...)...)
		if res.err == nil || !strings.Contains(res.err.Error(), tt.wantErr) {
			t.Errorf("%v: err = %v, want containing %q", tt.args, res.err, tt.wantErr)
		}
		if res.stdout != "" {
			t.Errorf("%v: unexpected output %q", tt.args, res.stdout)
		}
	}

	c := newCluster(t, false)
	if res := run(t, c, "", "-A", "--prometheus-service", "nonsense"); res.err == nil || !strings.Contains(res.err.Error(), "--prometheus-service") {
		t.Errorf("err = %v", res.err)
	}
	if res := run(t, c, "", "-A", "--prometheus-url", "localhost:9090"); res.err == nil || !strings.Contains(res.err.Error(), "invalid Prometheus URL") {
		t.Errorf("err = %v", res.err)
	}
	if res := run(t, c, "", "-A", "--prometheus-url", c.prom.URL, "--prometheus-token-file", filepath.Join(dir, "nope")); res.err == nil || !strings.Contains(res.err.Error(), "--prometheus-token-file") {
		t.Errorf("err = %v", res.err)
	}
}

func TestDemo(t *testing.T) {
	res := run(t, nil, "", "--demo")
	if res.err != nil {
		t.Fatal(res.err)
	}
	for _, want := range []string{
		"NAMESPACE", "embed-api (x4)", "MIG 1g.10gb slice", "idle 7+ days, reclaim", "time-slice, 3 per GPU",
		"smaller GPU: L4 (24 GB)", "low utilization, review", "1x A100 MIG 3g.40gb", "no data", "9 workloads, 19.5 GPUs",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("demo lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "\x1b[") {
		t.Error("colors written to a non-terminal")
	}

	// A longer window sees the notebook's last activity nine days ago.
	res = run(t, nil, "", "--demo", "--window", "14d")
	if res.err != nil || !strings.Contains(res.stdout, "idle 9 days, reclaim") || !strings.Contains(res.stdout, "Window 14d") {
		t.Errorf("err=%v output:\n%s", res.err, res.stdout)
	}

	res = run(t, nil, "", "--demo", "--color", "always")
	if res.err != nil || !strings.Contains(res.stdout, "\x1b[31midle 7+ days, reclaim\x1b[0m") {
		t.Errorf("err=%v output:\n%q", res.err, res.stdout)
	}

	for _, format := range report.Formats {
		res := run(t, nil, "", "--demo", "-o", format, "--explain")
		if res.err != nil || len(res.stdout) < 500 {
			t.Errorf("%s: err=%v, %d bytes", format, res.err, len(res.stdout))
		}
	}
	var rep report.Report
	if err := json.Unmarshal([]byte(run(t, nil, "", "--demo", "-o", "json").stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 9 || rep.Source != "bundled sample data (--demo)" {
		t.Errorf("demo JSON: %d rows, source %q", len(rep.Rows), rep.Source)
	}
}

func TestModelsCommand(t *testing.T) {
	res := run(t, nil, "", "models", "--price", "h100=4.25")
	if res.err != nil {
		t.Fatal(res.err)
	}
	lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
	if !strings.HasPrefix(lines[0], "MODEL") || len(lines) < 12 {
		t.Fatalf("output:\n%s", res.stdout)
	}
	fields := func(prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix+" ") {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		return ""
	}
	if got := fields("a100-80gb"); got != "a100-80gb A100 80GB 80 GB 5.07 built-in estimate 1g.10gb, 1g.20gb, 2g.20gb, 3g.40gb, 4g.40gb, 7g.80gb" {
		t.Errorf("a100-80gb line = %q", got)
	}
	if got := fields("h100"); !strings.Contains(got, "4.25 yours") {
		t.Errorf("h100 line = %q", got)
	}
	if got := fields("t4"); got != "t4 T4 16 GB 0.53 built-in estimate -" {
		t.Errorf("t4 line = %q", got)
	}
	if res := run(t, nil, "", "models", "--price", "nope=1"); res.err == nil {
		t.Error("bad price should fail")
	}

	// Custom prices change the footer note of the report.
	res = run(t, nil, "", "--demo", "--price", "a100=3.67")
	if res.err != nil || !strings.Contains(res.stdout, report.CustomPriceNote) || strings.Contains(res.stdout, report.PriceNote) {
		t.Errorf("err=%v output:\n%s", res.err, res.stdout)
	}
}

func TestVersionAndHelp(t *testing.T) {
	res := run(t, nil, "", "version")
	if res.err != nil || !strings.HasPrefix(res.stdout, "kubectl-gpu-fit dev (commit none") {
		t.Errorf("err=%v output %q", res.err, res.stdout)
	}
	res = run(t, nil, "", "--version")
	if res.err != nil || !strings.HasPrefix(res.stdout, "kubectl-gpu-fit dev") {
		t.Errorf("err=%v output %q", res.err, res.stdout)
	}
	res = run(t, nil, "", "--help")
	for _, want := range []string{"kubectl gpu-fit [flags]", "--prometheus-url", "--all-namespaces", "--kubeconfig", "--window", "--demo", "Nothing in the cluster is ever modified."} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	good := map[string]time.Duration{
		"7d": 7 * 24 * time.Hour, "36h": 36 * time.Hour, "2w": 14 * 24 * time.Hour, "1d12h": 36 * time.Hour,
		"90m": 90 * time.Minute, "30s": 30 * time.Second, "1.5H": 90 * time.Minute,
	}
	for in, want := range good {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if got, err := parseDuration(" 12h\n"); err != nil || got != 12*time.Hour {
		t.Errorf("surrounding whitespace: %v, %v", got, err)
	}
	for _, bad := range []string{"", "7", "d", "7x", "-1d", "1d-", "0s"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("parseDuration(%q) should fail", bad)
		}
	}
	for d, want := range map[time.Duration]string{7 * 24 * time.Hour: "7d", 36 * time.Hour: "36h", 90 * time.Minute: "90m", 90 * time.Second: "1m30s"} {
		if got := formatWindow(d); got != want {
			t.Errorf("formatWindow(%v) = %q, want %q", d, got, want)
		}
	}
	for in, want := range map[string]float64{"20%": 0.2, "0.2": 0.2, "0": 0, "35 %": 0.35, "1.5": 1.5} {
		if got, err := parseFraction(in); err != nil || got != want {
			t.Errorf("parseFraction(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// TestEndToEndRealRESTClient drives the command exactly as main does: real
// kubeconfig loading, the real client-go REST client and the real service
// proxy URL, against an HTTP imitation of the API server.
func TestEndToEndRealRESTClient(t *testing.T) {
	objs, series := testcluster.New(time.Now(), true)
	fakeProm := promtest.New("7d", series)
	defer fakeProm.Close()
	api := kubetest.NewAPIServer(objs...)
	api.Backends[testcluster.PrometheusNamespace+"/"+testcluster.PrometheusService] = fakeProm.Handler()
	srv := httptest.NewServer(api)
	defer srv.Close()

	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(testcluster.Kubeconfig(srv.URL, "notebooks")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvPrometheusURL, "")
	exec := func(args ...string) (string, error) {
		var out, errOut bytes.Buffer
		root := NewRootCommand(&out, &errOut)
		root.SetArgs(append([]string{"--kubeconfig", kubeconfig}, args...))
		err := root.Execute()
		return out.String(), err
	}

	out, err := exec("-A")
	if err != nil {
		t.Fatal(err)
	}
	if out != wantClusterTable {
		t.Errorf("output:\n%s\nwant:\n%s", out, wantClusterTable)
	}

	// Without -A the namespace of the kubeconfig context applies.
	out, err = exec()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "notebook-priya") || strings.Contains(out, "embed-api") || !strings.Contains(out, "namespace notebooks") {
		t.Errorf("output:\n%s", out)
	}
	// -n overrides it.
	if out, err = exec("-n", "ml-training"); err != nil || !strings.Contains(out, "train-nightly") || strings.Contains(out, "notebook-priya") {
		t.Errorf("err=%v output:\n%s", err, out)
	}

	var sawProxy, sawPods bool
	for _, req := range api.Requests() {
		if !strings.HasPrefix(req, "GET ") {
			t.Errorf("non-GET request to the API server: %s", req)
		}
		sawProxy = sawProxy || req == "GET /api/v1/namespaces/monitoring/services/kube-prometheus-stack-prometheus:http-web/proxy/api/v1/query"
		sawPods = sawPods || req == "GET /api/v1/pods"
	}
	if !sawProxy || !sawPods {
		t.Errorf("expected requests missing; got:\n%s", strings.Join(api.Requests(), "\n"))
	}
}
