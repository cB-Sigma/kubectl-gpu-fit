package prom

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ServiceRef identifies a Service port reachable through the API server
// proxy.
type ServiceRef struct {
	Namespace string
	Name      string
	// Port is a port name or number.
	Port string
	// Scheme is "http" or "https".
	Scheme string
}

func (r ServiceRef) String() string {
	s := fmt.Sprintf("%s/%s:%s", r.Namespace, r.Name, r.Port)
	if r.Scheme == "https" {
		s = "https:" + s
	}
	return s
}

// ParseServiceRef parses "[https:]namespace/name[:port]". The port defaults
// to 9090.
func ParseServiceRef(s string) (ServiceRef, error) {
	ref := ServiceRef{Scheme: "http", Port: "9090"}
	rest := strings.TrimSpace(s)
	for _, scheme := range []string{"http", "https"} {
		if strings.HasPrefix(rest, scheme+":") && strings.Contains(rest[len(scheme)+1:], "/") {
			ref.Scheme = scheme
			rest = rest[len(scheme)+1:]
			break
		}
	}
	ns, name, ok := strings.Cut(rest, "/")
	if !ok || ns == "" || name == "" {
		return ref, fmt.Errorf("invalid service %q: want [https:]namespace/name[:port]", s)
	}
	if n, port, hasPort := strings.Cut(name, ":"); hasPort {
		if n == "" || port == "" {
			return ref, fmt.Errorf("invalid service %q: want [https:]namespace/name[:port]", s)
		}
		name, ref.Port = n, port
	}
	ref.Namespace, ref.Name = ns, name
	return ref, nil
}

// ProxyTransport reaches Prometheus through the Kubernetes API server's
// service proxy (GET /api/v1/namespaces/NS/services/NAME:PORT/proxy/...), so
// no port-forward or ingress is needed.
type ProxyTransport struct {
	client kubernetes.Interface
	ref    ServiceRef
	// PathPrefix is prepended to API paths for servers running with a
	// route prefix.
	PathPrefix string
}

// NewProxyTransport returns a transport for the given service.
func NewProxyTransport(client kubernetes.Interface, ref ServiceRef) *ProxyTransport {
	return &ProxyTransport{client: client, ref: ref}
}

// Get implements Transport.
func (t *ProxyTransport) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	flat := make(map[string]string, len(params))
	for k, v := range params {
		if len(v) > 0 {
			flat[k] = v[0]
		}
	}
	if t.PathPrefix != "" {
		path = strings.Trim(t.PathPrefix, "/") + "/" + strings.TrimLeft(path, "/")
	}
	scheme := t.ref.Scheme
	if scheme == "http" {
		scheme = ""
	}
	wrapper := t.client.CoreV1().Services(t.ref.Namespace).ProxyGet(scheme, t.ref.Name, t.ref.Port, path, flat)
	if wrapper == nil {
		return nil, fmt.Errorf("service proxy is not available for %s", t)
	}
	body, err := wrapper.DoRaw(ctx)
	if err != nil {
		// Prometheus reports query errors as JSON with a 4xx/5xx status;
		// prefer its message over the generic proxy error.
		if perr := prometheusError(body); perr != nil {
			return nil, perr
		}
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf("not allowed to reach %s through the API server proxy: this needs \"get\" on \"services/proxy\" for %q in namespace %q (see deploy/rbac.yaml), or use --prometheus-url: %w",
				t, t.ref.Name+":"+t.ref.Port, t.ref.Namespace, err)
		}
		return nil, fmt.Errorf("querying Prometheus via the API server proxy (%s): %w", t, err)
	}
	return body, nil
}

func (t *ProxyTransport) String() string { return "service " + t.ref.String() }

// fallbackNamespaces are searched when listing Services cluster-wide is not
// permitted.
var fallbackNamespaces = []string{
	"monitoring", "prometheus", "observability", "kube-prometheus-stack",
	"gpu-operator", "cattle-monitoring-system", "kube-system", "default",
}

var excludedNameParts = []string{
	"alertmanager", "exporter", "kube-state-metrics", "pushgateway",
	"operator", "adapter", "grafana", "kubelet", "thanos-ruler", "webhook",
}

var preferredNames = map[string]bool{
	"prometheus": true, "prometheus-k8s": true, "prometheus-server": true,
	"kube-prometheus-stack-prometheus": true, "prometheus-kube-prometheus-prometheus": true,
	"rancher-monitoring-prometheus": true,
}

var preferredPortNames = map[string]bool{"web": true, "http-web": true, "http": true, "prometheus": true}

type scored struct {
	ref   ServiceRef
	score int
}

// Discover looks for Prometheus Services and returns candidates, best first.
func Discover(ctx context.Context, client kubernetes.Interface) ([]ServiceRef, error) {
	var services []corev1.Service
	list, err := client.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err == nil {
		services = list.Items
	} else {
		var lastErr = err
		found := false
		for _, ns := range fallbackNamespaces {
			l, nsErr := client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
			if nsErr != nil {
				lastErr = nsErr
				continue
			}
			found = true
			services = append(services, l.Items...)
		}
		if !found {
			return nil, fmt.Errorf("listing services to find Prometheus: %w", lastErr)
		}
	}

	var cands []scored
	for i := range services {
		if c, ok := scoreService(&services[i]); ok {
			cands = append(cands, c)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].ref.String() < cands[j].ref.String()
	})
	out := make([]ServiceRef, len(cands))
	for i, c := range cands {
		out[i] = c.ref
	}
	return out, nil
}

func scoreService(svc *corev1.Service) (scored, bool) {
	if svc.Spec.Type == corev1.ServiceTypeExternalName {
		return scored{}, false
	}
	name := strings.ToLower(svc.Name)
	for _, part := range excludedNameParts {
		if strings.Contains(name, part) {
			return scored{}, false
		}
	}
	score := 0
	labels := svc.Labels
	if labels["app.kubernetes.io/name"] == "prometheus" || labels["app"] == "prometheus" ||
		labels["app"] == "kube-prometheus-stack-prometheus" {
		score += 50
	}
	switch {
	case preferredNames[name]:
		score += 40
	case name == "prometheus-operated":
		// Headless governing service of the operator: works, but the
		// regular Service is the better target when both exist.
		score += 25
	case strings.Contains(name, "prometheus"):
		score += 20
	}
	if score == 0 {
		return scored{}, false
	}

	var port *corev1.ServicePort
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			continue
		}
		if p.Port == 9090 {
			port = p
			break
		}
		if port == nil && preferredPortNames[p.Name] {
			port = p
		}
	}
	if port == nil {
		if len(svc.Spec.Ports) != 1 {
			return scored{}, false
		}
		port = &svc.Spec.Ports[0]
		score -= 10
	}
	ref := ServiceRef{Namespace: svc.Namespace, Name: svc.Name, Scheme: "http", Port: strconv.Itoa(int(port.Port))}
	if port.Name != "" {
		ref.Port = port.Name
	}
	if strings.HasPrefix(port.Name, "https") || port.Port == 443 {
		ref.Scheme = "https"
	}
	return scored{ref: ref, score: score}, true
}
