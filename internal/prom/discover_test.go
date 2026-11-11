package prom_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/prom"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/promtest"
)

func svc(ns, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

func port(name string, number int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: number}
}

func TestParseServiceRef(t *testing.T) {
	tests := map[string]prom.ServiceRef{
		"monitoring/prometheus-k8s":       {Namespace: "monitoring", Name: "prometheus-k8s", Port: "9090", Scheme: "http"},
		"monitoring/prometheus-k8s:web":   {Namespace: "monitoring", Name: "prometheus-k8s", Port: "web", Scheme: "http"},
		"https:obs/prom:9091":             {Namespace: "obs", Name: "prom", Port: "9091", Scheme: "https"},
		"http:monitoring/prometheus:8080": {Namespace: "monitoring", Name: "prometheus", Port: "8080", Scheme: "http"},
		"http/prometheus:9090":            {Namespace: "http", Name: "prometheus", Port: "9090", Scheme: "http"},
	}
	for in, want := range tests {
		got, err := prom.ParseServiceRef(in)
		if err != nil || got != want {
			t.Errorf("ParseServiceRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "prometheus", "/name", "ns/", "ns/name:", "ns/:9090"} {
		if _, err := prom.ParseServiceRef(bad); err == nil {
			t.Errorf("ParseServiceRef(%q) should fail", bad)
		}
	}
	if s := (prom.ServiceRef{Namespace: "a", Name: "b", Port: "web", Scheme: "https"}).String(); s != "https:a/b:web" {
		t.Errorf("String() = %s", s)
	}
}

func TestDiscoverRanking(t *testing.T) {
	promLabels := map[string]string{"app.kubernetes.io/name": "prometheus"}
	client := fake.NewSimpleClientset(
		svc("monitoring", "kube-prometheus-stack-prometheus", promLabels, port("http-web", 9090), port("reloader-web", 8080)),
		svc("monitoring", "prometheus-operated", promLabels, port("http-web", 9090)),
		svc("monitoring", "kube-prometheus-stack-alertmanager", nil, port("http-web", 9093)),
		svc("monitoring", "kube-prometheus-stack-operator", nil, port("https", 443)),
		svc("monitoring", "kube-prometheus-stack-prometheus-node-exporter", nil, port("http-metrics", 9100)),
		svc("monitoring", "kube-prometheus-stack-kube-state-metrics", nil, port("http", 8080)),
		svc("monitoring", "grafana", nil, port("http", 80)),
		svc("legacy", "prometheus-server", nil, port("http", 80)),
		svc("odd", "my-prometheus", nil, port("", 9090)),
		svc("odd", "prometheus-multi", nil, port("a", 1), port("b", 2)),
		svc("gpu-operator", "nvidia-dcgm-exporter", nil, port("gpu-metrics", 9400)),
		svc("default", "kubernetes", nil, port("https", 443)),
	)
	got, err := prom.Discover(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range got {
		names = append(names, r.String())
	}
	want := []string{
		"monitoring/kube-prometheus-stack-prometheus:http-web",
		"monitoring/prometheus-operated:http-web",
		"legacy/prometheus-server:http",
		"odd/my-prometheus:9090",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("candidates:\n got %v\nwant %v", names, want)
	}
}

func TestDiscoverFallsBackToWellKnownNamespaces(t *testing.T) {
	client := fake.NewSimpleClientset(
		svc("monitoring", "prometheus-k8s", nil, port("web", 9090)),
		svc("elsewhere", "prometheus", nil, port("web", 9090)),
	)
	client.PrependReactor("list", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == metav1.NamespaceAll {
			return true, nil, errors.New("services is forbidden at the cluster scope")
		}
		return false, nil, nil
	})
	got, err := prom.Discover(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "monitoring/prometheus-k8s:web" {
		t.Errorf("got %v", got)
	}

	client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := prom.Discover(context.Background(), client); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want forbidden", err)
	}
}

func TestProxyTransport(t *testing.T) {
	fakeProm := promtest.New("7d", []promtest.Series{
		{Metric: "DCGM_FI_DEV_GPU_UTIL", Labels: map[string]string{"pod": "p"}, Avg: 12},
	})
	defer fakeProm.Close()
	proxy := promtest.NewProxy()
	proxy.Register("monitoring", "prometheus-k8s", fakeProm.Handler())
	client := fake.NewSimpleClientset()
	client.AddProxyReactor("services", proxy.React)

	ref := prom.ServiceRef{Namespace: "monitoring", Name: "prometheus-k8s", Port: "web", Scheme: "http"}
	c := prom.NewClient(prom.NewProxyTransport(client, ref))
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := c.Query(context.Background(), "avg_over_time(DCGM_FI_DEV_GPU_UTIL[7d])", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Value != 12 {
		t.Errorf("result = %+v", res)
	}
	calls := proxy.Calls()
	if len(calls) != 2 || calls[0].Path != "api/v1/query" || calls[0].Port != "web" || calls[0].Scheme != "" {
		t.Errorf("calls = %+v", calls)
	}
	if c.Endpoint() != "service monitoring/prometheus-k8s:web" {
		t.Errorf("Endpoint() = %q", c.Endpoint())
	}

	// A PromQL error surfaces with Prometheus' own message.
	if _, err := c.Query(context.Background(), "nonsense(", time.Now()); err == nil || !strings.Contains(err.Error(), "does not understand") {
		t.Errorf("err = %v", err)
	}
	// A service without a backend reports the proxy failure.
	missing := prom.NewClient(prom.NewProxyTransport(client, prom.ServiceRef{Namespace: "x", Name: "gone", Port: "9090", Scheme: "https"}))
	if err := missing.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "API server proxy") {
		t.Errorf("err = %v", err)
	}
}
