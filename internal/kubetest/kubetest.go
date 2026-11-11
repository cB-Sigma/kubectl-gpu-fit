// Package kubetest builds Kubernetes objects for tests.
package kubetest

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func controller(kind, name string) []metav1.OwnerReference {
	t := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &t}}
}

// Pod returns a running pod on node "node-1" whose single container has the
// given resource limits, e.g. Pod("ns", "p", "nvidia.com/gpu", 1).
func Pod(namespace, name, resourceName string, count int64) *corev1.Pod {
	limits := corev1.ResourceList{}
	if resourceName != "" {
		limits[corev1.ResourceName(resourceName)] = *resource.NewQuantity(count, resource.DecimalSI)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{Limits: limits}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// OwnedBy sets the controlling owner of a pod and returns it.
func OwnedBy(p *corev1.Pod, kind, name string) *corev1.Pod {
	p.OwnerReferences = controller(kind, name)
	return p
}

// OnNode sets the node of a pod and returns it.
func OnNode(p *corev1.Pod, node string) *corev1.Pod {
	p.Spec.NodeName = node
	return p
}

// ReplicaSet returns a ReplicaSet owned by a Deployment (or by nothing if
// deployment is empty).
func ReplicaSet(namespace, name, deployment string) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if deployment != "" {
		rs.OwnerReferences = controller("Deployment", deployment)
	}
	return rs
}

// Job returns a Job owned by a CronJob (or by nothing if cronJob is empty).
func Job(namespace, name, cronJob string) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if cronJob != "" {
		j.OwnerReferences = controller("CronJob", cronJob)
	}
	return j
}

// Node returns a node with the given labels.
func Node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}
