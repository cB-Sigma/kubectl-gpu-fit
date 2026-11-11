// Package kube reads GPU-requesting pods from the Kubernetes API and groups
// them into workloads by their owning controller. It only ever issues get
// and list requests.
package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ResourceGPU is the extended resource for a whole (or, on nodes
	// configured for sharing, a shared) NVIDIA GPU.
	ResourceGPU = "nvidia.com/gpu"
	// ResourceGPUShared is advertised by the device plugin for time-sliced
	// or MPS replicas when renameByDefault is set.
	ResourceGPUShared = "nvidia.com/gpu.shared"
	// ResourceMIGPrefix prefixes MIG resources under the "mixed" strategy,
	// e.g. nvidia.com/mig-1g.10gb.
	ResourceMIGPrefix = "nvidia.com/mig-"

	labelProduct         = "nvidia.com/gpu.product"
	labelMemory          = "nvidia.com/gpu.memory"
	labelReplicas        = "nvidia.com/gpu.replicas"
	labelSharingStrategy = "nvidia.com/gpu.sharing-strategy"
)

// Request is the GPU resource one pod asks for.
type Request struct {
	// Resource is the extended resource name.
	Resource string `json:"resource"`
	// Count is the number of devices summed over the pod's containers.
	Count int `json:"count"`
}

// MIGProfile returns the profile name for nvidia.com/mig-* resources.
func (r Request) MIGProfile() string {
	if strings.HasPrefix(r.Resource, ResourceMIGPrefix) {
		return strings.TrimSuffix(strings.TrimPrefix(r.Resource, ResourceMIGPrefix), ".shared")
	}
	return ""
}

// Shared reports whether the resource name itself marks a shared GPU.
func (r Request) Shared() bool { return strings.HasSuffix(r.Resource, ".shared") }

// NodeInfo carries the GPU Feature Discovery labels of a node.
type NodeInfo struct {
	Name string `json:"name"`
	// Product is nvidia.com/gpu.product, e.g. "NVIDIA-A100-SXM4-80GB".
	Product string `json:"product,omitempty"`
	// MemoryMiB is nvidia.com/gpu.memory.
	MemoryMiB float64 `json:"memoryMiB,omitempty"`
	// SharingStrategy is nvidia.com/gpu.sharing-strategy ("none",
	// "time-slicing" or "mps"); empty if the label is absent.
	SharingStrategy string `json:"sharingStrategy,omitempty"`
	// Replicas is nvidia.com/gpu.replicas: how many pods share each GPU.
	Replicas int `json:"replicas,omitempty"`
}

// Pod is a running pod that requests NVIDIA GPU resources.
type Pod struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Node      string    `json:"node,omitempty"`
	Created   time.Time `json:"created"`
	Request   Request   `json:"request"`
}

// Workload is a set of pods with the same owning controller.
type Workload struct {
	Namespace string `json:"namespace"`
	// Kind is Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod or the
	// kind of a custom controller.
	Kind string `json:"kind"`
	Name string `json:"name"`
	Pods []Pod  `json:"pods"`
}

// Snapshot is everything read from the cluster.
type Snapshot struct {
	Workloads []Workload
	// Nodes is keyed by node name; empty when nodes could not be listed.
	Nodes map[string]NodeInfo
	// Warnings are non-fatal problems (missing permissions and the like).
	Warnings []string
}

// Load lists GPU pods in the namespace (all namespaces if empty), resolves
// their owners and reads node GPU labels.
func Load(ctx context.Context, client kubernetes.Interface, namespace string) (*Snapshot, error) {
	snap := &Snapshot{Nodes: map[string]NodeInfo{}}

	var pods []corev1.Pod
	cont := ""
	for {
		list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Running",
			Limit:         500,
			Continue:      cont,
		})
		if err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}
		pods = append(pods, list.Items...)
		cont = list.Continue
		if cont == "" {
			break
		}
	}

	r := &ownerResolver{ctx: ctx, client: client, cache: map[string]owner{}}
	groups := map[string]*Workload{}
	for i := range pods {
		p := &pods[i]
		// The fake clientset ignores field selectors, and a pod can change
		// phase between pages; filter again.
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		req, ok := GPURequest(p)
		if !ok {
			continue
		}
		o := r.resolve(p)
		key := p.Namespace + "/" + o.kind + "/" + o.name
		w := groups[key]
		if w == nil {
			w = &Workload{Namespace: p.Namespace, Kind: o.kind, Name: o.name}
			groups[key] = w
		}
		w.Pods = append(w.Pods, Pod{
			Namespace: p.Namespace,
			Name:      p.Name,
			Node:      p.Spec.NodeName,
			Created:   p.CreationTimestamp.Time,
			Request:   req,
		})
	}
	snap.Warnings = append(snap.Warnings, r.warnings()...)

	for _, w := range groups {
		sort.Slice(w.Pods, func(i, j int) bool { return w.Pods[i].Name < w.Pods[j].Name })
		snap.Workloads = append(snap.Workloads, *w)
	}
	sort.Slice(snap.Workloads, func(i, j int) bool {
		a, b := snap.Workloads[i], snap.Workloads[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})

	if len(snap.Workloads) > 0 {
		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("could not list nodes (%v); GPU models come from DCGM labels only", err))
		} else {
			for i := range nodes.Items {
				n := &nodes.Items[i]
				if info, ok := nodeInfo(n); ok {
					snap.Nodes[n.Name] = info
				}
			}
		}
	}
	return snap, nil
}

func nodeInfo(n *corev1.Node) (NodeInfo, bool) {
	l := n.Labels
	if l[labelProduct] == "" && l[labelSharingStrategy] == "" {
		return NodeInfo{}, false
	}
	info := NodeInfo{Name: n.Name, Product: l[labelProduct], SharingStrategy: l[labelSharingStrategy]}
	if v, err := strconv.ParseFloat(l[labelMemory], 64); err == nil {
		info.MemoryMiB = v
	}
	if v, err := strconv.Atoi(l[labelReplicas]); err == nil {
		info.Replicas = v
	}
	return info, true
}

// GPURequest returns the NVIDIA GPU resource a pod requests. Limits are
// authoritative for extended resources; init containers are ignored because
// they do not hold a GPU while the pod runs (sidecar-style init containers
// requesting GPUs are rare enough to leave out).
func GPURequest(p *corev1.Pod) (Request, bool) {
	totals := map[string]int64{}
	for _, c := range p.Spec.Containers {
		for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
			seen := false
			for name, qty := range list {
				n := string(name)
				if n == ResourceGPU || n == ResourceGPUShared || strings.HasPrefix(n, ResourceMIGPrefix) {
					if v := qty.Value(); v > 0 {
						totals[n] += v
						seen = true
					}
				}
			}
			if seen {
				break // do not count requests on top of limits
			}
		}
	}
	if len(totals) == 0 {
		return Request{}, false
	}
	// A pod mixing GPU resource types is unusual; report the largest.
	names := make([]string, 0, len(totals))
	for n := range totals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if totals[names[i]] != totals[names[j]] {
			return totals[names[i]] > totals[names[j]]
		}
		return names[i] < names[j]
	})
	return Request{Resource: names[0], Count: int(totals[names[0]])}, true
}

type owner struct{ kind, name string }

type ownerResolver struct {
	ctx    context.Context //nolint:containedctx // short-lived helper scoped to one Load call
	client kubernetes.Interface
	cache  map[string]owner
	warned map[string]string
}

func (r *ownerResolver) warnings() []string {
	out := make([]string, 0, len(r.warned))
	for _, w := range r.warned {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func (r *ownerResolver) warn(kind string, err error) {
	if r.warned == nil {
		r.warned = map[string]string{}
	}
	if _, ok := r.warned[kind]; !ok {
		r.warned[kind] = fmt.Sprintf("could not read %s (%v); workload names are inferred from pod owners", kind, err)
	}
}

var (
	// podTemplateHash matches the suffix a Deployment adds to its
	// ReplicaSets.
	podTemplateHash = regexp.MustCompile(`^(.+)-[a-z0-9]{6,10}$`)
	// cronJobSuffix matches the scheduled-time suffix a CronJob adds to its
	// Jobs (minutes since the epoch).
	cronJobSuffix = regexp.MustCompile(`^(.+)-\d{8,10}$`)
)

func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

func (r *ownerResolver) resolve(p *corev1.Pod) owner {
	ref := controllerOf(p.OwnerReferences)
	if ref == nil {
		return owner{"Pod", p.Name}
	}
	switch ref.Kind {
	case "ReplicaSet":
		return r.cached(p.Namespace, ref, r.replicaSetOwner)
	case "Job":
		return r.cached(p.Namespace, ref, r.jobOwner)
	default:
		return owner{ref.Kind, ref.Name}
	}
}

func (r *ownerResolver) cached(ns string, ref *metav1.OwnerReference, lookup func(ns, name string) owner) owner {
	key := ns + "/" + ref.Kind + "/" + ref.Name
	if o, ok := r.cache[key]; ok {
		return o
	}
	o := lookup(ns, ref.Name)
	r.cache[key] = o
	return o
}

func (r *ownerResolver) replicaSetOwner(ns, name string) owner {
	rs, err := r.client.AppsV1().ReplicaSets(ns).Get(r.ctx, name, metav1.GetOptions{})
	if err != nil {
		r.warn("replicasets", err)
		// Without access, fall back on the naming convention.
		if m := podTemplateHash.FindStringSubmatch(name); m != nil {
			return owner{"Deployment", m[1]}
		}
		return owner{"ReplicaSet", name}
	}
	if ref := controllerOf(rs.OwnerReferences); ref != nil {
		return owner{ref.Kind, ref.Name}
	}
	return owner{"ReplicaSet", name}
}

func (r *ownerResolver) jobOwner(ns, name string) owner {
	job, err := r.client.BatchV1().Jobs(ns).Get(r.ctx, name, metav1.GetOptions{})
	if err != nil {
		r.warn("jobs", err)
		if m := cronJobSuffix.FindStringSubmatch(name); m != nil {
			return owner{"CronJob", m[1]}
		}
		return owner{"Job", name}
	}
	if ref := controllerOf(job.OwnerReferences); ref != nil {
		return owner{ref.Kind, ref.Name}
	}
	return owner{"Job", name}
}
