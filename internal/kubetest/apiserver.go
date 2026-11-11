package kubetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// APIServer is a tiny read-only imitation of the Kubernetes API server. It
// serves just the endpoints kubectl-gpu-fit uses, so that the real client-go
// REST plumbing (including the service proxy path) can be exercised without
// a cluster.
type APIServer struct {
	Pods        []corev1.Pod
	Services    []corev1.Service
	Nodes       []corev1.Node
	ReplicaSets []appsv1.ReplicaSet
	Jobs        []batchv1.Job
	// Backends maps "namespace/service" to the handler reached through
	// .../services/NAME:PORT/proxy/.
	Backends map[string]http.Handler

	mu       sync.Mutex
	requests []string
}

// NewAPIServer sorts the given objects into an APIServer.
func NewAPIServer(objs ...runtime.Object) *APIServer {
	s := &APIServer{Backends: map[string]http.Handler{}}
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.Pod:
			s.Pods = append(s.Pods, *v)
		case *corev1.Service:
			s.Services = append(s.Services, *v)
		case *corev1.Node:
			s.Nodes = append(s.Nodes, *v)
		case *appsv1.ReplicaSet:
			s.ReplicaSets = append(s.ReplicaSets, *v)
		case *batchv1.Job:
			s.Jobs = append(s.Jobs, *v)
		default:
			panic(fmt.Sprintf("kubetest: unsupported object %T", o))
		}
	}
	return s
}

// Requests returns "METHOD path" for every request received.
func (s *APIServer) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *APIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()

	if r.Method != http.MethodGet {
		writeStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "this fake API server is read-only")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Strip the group/version prefix: api/v1 or apis/GROUP/VERSION.
	switch {
	case len(parts) >= 2 && parts[0] == "api":
		parts = parts[2:]
	case len(parts) >= 3 && parts[0] == "apis":
		parts = parts[3:]
	default:
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown path "+r.URL.Path)
		return
	}
	namespace := ""
	if len(parts) >= 3 && parts[0] == "namespaces" {
		namespace, parts = parts[1], parts[2:]
	}
	if len(parts) == 0 {
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown path "+r.URL.Path)
		return
	}
	resource, rest := parts[0], parts[1:]

	switch {
	case resource == "pods" && len(rest) == 0:
		list := corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
		phase := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "status.phase=")
		for _, p := range s.Pods {
			if (namespace == "" || p.Namespace == namespace) && (phase == "" || string(p.Status.Phase) == phase) {
				list.Items = append(list.Items, p)
			}
		}
		writeJSON(w, list)
	case resource == "services" && len(rest) == 0:
		list := corev1.ServiceList{TypeMeta: metav1.TypeMeta{Kind: "ServiceList", APIVersion: "v1"}}
		for _, svc := range s.Services {
			if namespace == "" || svc.Namespace == namespace {
				list.Items = append(list.Items, svc)
			}
		}
		writeJSON(w, list)
	case resource == "services" && len(rest) >= 2 && rest[1] == "proxy":
		s.proxy(w, r, namespace, rest[0], strings.Join(rest[2:], "/"))
	case resource == "nodes" && len(rest) == 0:
		writeJSON(w, corev1.NodeList{TypeMeta: metav1.TypeMeta{Kind: "NodeList", APIVersion: "v1"}, Items: s.Nodes})
	case resource == "replicasets" && len(rest) == 1:
		for _, rs := range s.ReplicaSets {
			if rs.Namespace == namespace && rs.Name == rest[0] {
				rs.TypeMeta = metav1.TypeMeta{Kind: "ReplicaSet", APIVersion: "apps/v1"}
				writeJSON(w, rs)
				return
			}
		}
		writeStatus(w, http.StatusNotFound, "NotFound", "replicaset not found")
	case resource == "jobs" && len(rest) == 1:
		for _, j := range s.Jobs {
			if j.Namespace == namespace && j.Name == rest[0] {
				j.TypeMeta = metav1.TypeMeta{Kind: "Job", APIVersion: "batch/v1"}
				writeJSON(w, j)
				return
			}
		}
		writeStatus(w, http.StatusNotFound, "NotFound", "job not found")
	default:
		writeStatus(w, http.StatusNotFound, "NotFound", "unknown path "+r.URL.Path)
	}
}

// proxy imitates the service proxy: the service segment is
// [scheme:]name[:port].
func (s *APIServer) proxy(w http.ResponseWriter, r *http.Request, namespace, segment, path string) {
	fields := strings.Split(segment, ":")
	name := fields[0]
	if len(fields) == 3 {
		name = fields[1]
	}
	backend := s.Backends[namespace+"/"+name]
	if backend == nil {
		writeStatus(w, http.StatusServiceUnavailable, "ServiceUnavailable", fmt.Sprintf("no endpoints available for service %q", name))
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + path
	backend.ServeHTTP(w, r2)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Code: int32(code), Reason: metav1.StatusReason(reason), Message: msg,
	})
}
