package kube_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kube"
	kt "github.com/cb-Sigma/kubectl-gpu-fit/internal/kubetest"
)

func objects() []runtime.Object {
	pending := kt.Pod("ml", "pending", kube.ResourceGPU, 1)
	pending.Status.Phase = corev1.PodPending
	done := kt.Pod("ml", "done", kube.ResourceGPU, 1)
	done.Status.Phase = corev1.PodSucceeded
	terminating := kt.Pod("ml", "terminating", kube.ResourceGPU, 1)
	terminating.DeletionTimestamp = &metav1.Time{}

	return []runtime.Object{
		// Deployment with two replicas, via a ReplicaSet.
		kt.OwnedBy(kt.Pod("ml", "embed-api-7c9d8b6f5-aaaaa", kube.ResourceGPU, 1), "ReplicaSet", "embed-api-7c9d8b6f5"),
		kt.OwnedBy(kt.Pod("ml", "embed-api-7c9d8b6f5-bbbbb", kube.ResourceGPU, 1), "ReplicaSet", "embed-api-7c9d8b6f5"),
		kt.ReplicaSet("ml", "embed-api-7c9d8b6f5", "embed-api"),
		// Stand-alone ReplicaSet.
		kt.OwnedBy(kt.Pod("ml", "raw-rs-xyz12", kube.ResourceGPU, 1), "ReplicaSet", "raw-rs"),
		kt.ReplicaSet("ml", "raw-rs", ""),
		// StatefulSet, DaemonSet.
		kt.OwnedBy(kt.Pod("ml", "train-0", kube.ResourceGPU, 4), "StatefulSet", "train"),
		kt.OwnedBy(kt.Pod("ml", "probe-abcde", kube.ResourceGPU, 1), "DaemonSet", "probe"),
		// CronJob -> Job -> Pod, and a plain Job.
		kt.OwnedBy(kt.Pod("batch", "nightly-28912345-zzzzz", kube.ResourceGPU, 2), "Job", "nightly-28912345"),
		kt.Job("batch", "nightly-28912345", "nightly"),
		kt.OwnedBy(kt.Pod("batch", "oneoff-qqqqq", kube.ResourceGPU, 1), "Job", "oneoff"),
		kt.Job("batch", "oneoff", ""),
		// Custom controller and a bare pod.
		kt.OwnedBy(kt.Pod("batch", "ray-worker-1", kube.ResourceGPU, 1), "RayCluster", "ray"),
		kt.Pod("notebooks", "notebook-priya", kube.ResourceGPU, 1),
		// MIG and shared resources.
		kt.Pod("ml", "mig-pod", "nvidia.com/mig-1g.10gb", 1),
		kt.Pod("ml", "shared-pod", kube.ResourceGPUShared, 1),
		// Things that must be ignored.
		kt.Pod("ml", "cpu-only", "", 0),
		kt.Pod("ml", "amd", "amd.com/gpu", 1),
		pending, done, terminating,

		kt.Node("node-1", map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB", "nvidia.com/gpu.memory": "81920",
			"nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing",
		}),
		kt.Node("cpu-node", map[string]string{"kubernetes.io/arch": "amd64"}),
	}
}

func describe(ws []kube.Workload) []string {
	var out []string
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%s/%s/%s x%d %s:%d", w.Namespace, w.Kind, w.Name, len(w.Pods), w.Pods[0].Request.Resource, w.Pods[0].Request.Count))
	}
	return out
}

func TestLoadGroupsPodsByOwner(t *testing.T) {
	client := fake.NewSimpleClientset(objects()...)
	snap, err := kube.Load(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"batch/CronJob/nightly x1 nvidia.com/gpu:2",
		"batch/Job/oneoff x1 nvidia.com/gpu:1",
		"batch/RayCluster/ray x1 nvidia.com/gpu:1",
		"ml/Deployment/embed-api x2 nvidia.com/gpu:1",
		"ml/Pod/mig-pod x1 nvidia.com/mig-1g.10gb:1",
		"ml/DaemonSet/probe x1 nvidia.com/gpu:1",
		"ml/ReplicaSet/raw-rs x1 nvidia.com/gpu:1",
		"ml/Pod/shared-pod x1 nvidia.com/gpu.shared:1",
		"ml/StatefulSet/train x1 nvidia.com/gpu:4",
		"notebooks/Pod/notebook-priya x1 nvidia.com/gpu:1",
	}
	got := describe(snap.Workloads)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("workloads:\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
	if len(snap.Warnings) != 0 {
		t.Errorf("warnings = %v", snap.Warnings)
	}
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes = %+v", snap.Nodes)
	}
	n := snap.Nodes["node-1"]
	if n.Product != "NVIDIA-A100-SXM4-80GB" || n.MemoryMiB != 81920 || n.Replicas != 4 || n.SharingStrategy != "time-slicing" {
		t.Errorf("node info = %+v", n)
	}
	for _, w := range snap.Workloads {
		if w.Name == "embed-api" {
			if w.Pods[0].Name != "embed-api-7c9d8b6f5-aaaaa" || w.Pods[0].Node != "node-1" {
				t.Errorf("pods = %+v", w.Pods)
			}
		}
	}

	// Only read verbs are ever used.
	for _, a := range client.Actions() {
		if v := a.GetVerb(); v != "list" && v != "get" {
			t.Errorf("unexpected %s on %s", v, a.GetResource().Resource)
		}
	}
}

func TestLoadNamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(objects()...)
	snap, err := kube.Load(context.Background(), client, "notebooks")
	if err != nil {
		t.Fatal(err)
	}
	if got := describe(snap.Workloads); len(got) != 1 || got[0] != "notebooks/Pod/notebook-priya x1 nvidia.com/gpu:1" {
		t.Errorf("got %v", got)
	}

	client = fake.NewSimpleClientset(objects()...)
	empty, err := kube.Load(context.Background(), client, "nothing-here")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Workloads) != 0 || len(empty.Nodes) != 0 {
		t.Errorf("got %+v", empty)
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "nodes" {
			t.Error("nodes listed although there are no GPU workloads")
		}
	}
}

func TestLoadDegradesWithoutOwnerAndNodeAccess(t *testing.T) {
	client := fake.NewSimpleClientset(objects()...)
	deny := func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	}
	client.PrependReactor("get", "replicasets", deny)
	client.PrependReactor("get", "jobs", deny)
	client.PrependReactor("list", "nodes", deny)

	snap, err := kube.Load(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(describe(snap.Workloads), "\n")
	for _, want := range []string{
		"ml/Deployment/embed-api x2", // inferred from the pod-template-hash suffix
		"ml/ReplicaSet/raw-rs x1",    // no hash suffix: left alone
		"batch/CronJob/nightly x1",   // inferred from the schedule suffix
		"batch/Job/oneoff x1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(snap.Warnings) != 3 {
		t.Errorf("want 3 warnings (replicasets, jobs, nodes), got %v", snap.Warnings)
	}
	if len(snap.Nodes) != 0 {
		t.Errorf("nodes = %v", snap.Nodes)
	}
}

func TestLoadFailsWithoutPodAccess(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pods is forbidden")
	})
	if _, err := kube.Load(context.Background(), client, ""); err == nil || !strings.Contains(err.Error(), "listing pods") {
		t.Errorf("err = %v", err)
	}
}

func TestGPURequest(t *testing.T) {
	qty := func(n int64) resource.Quantity { return *resource.NewQuantity(n, resource.DecimalSI) }
	gpu := corev1.ResourceName(kube.ResourceGPU)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{gpu: qty(8)}}}},
		Containers: []corev1.Container{
			// Limits and requests are the same GPUs, not additional ones.
			{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{gpu: qty(2)}, Requests: corev1.ResourceList{gpu: qty(2)}}},
			// Requests only.
			{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{gpu: qty(1)}}},
			{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: qty(4)}}},
		},
	}}
	req, ok := kube.GPURequest(pod)
	if !ok || req.Resource != kube.ResourceGPU || req.Count != 3 {
		t.Errorf("got %+v, %v; want 3 GPUs", req, ok)
	}

	if _, ok := kube.GPURequest(kt.Pod("a", "b", kube.ResourceGPU, 0)); ok {
		t.Error("zero GPUs should not count as a GPU pod")
	}

	for res, want := range map[string]string{
		"nvidia.com/mig-1g.10gb":        "1g.10gb",
		"nvidia.com/mig-3g.40gb.shared": "3g.40gb",
		kube.ResourceGPU:                "",
		kube.ResourceGPUShared:          "",
	} {
		r := kube.Request{Resource: res, Count: 1}
		if got := r.MIGProfile(); got != want {
			t.Errorf("MIGProfile(%s) = %q, want %q", res, got, want)
		}
	}
	if !(kube.Request{Resource: kube.ResourceGPUShared}).Shared() || (kube.Request{Resource: kube.ResourceGPU}).Shared() {
		t.Error("Shared() wrong")
	}
}
