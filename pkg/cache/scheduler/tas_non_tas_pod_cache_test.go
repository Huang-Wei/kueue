/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduler

import (
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/kueue/pkg/resources"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
)

func TestNonTasUsageCachePriorityThreshold(t *testing.T) {
	cases := map[string]struct {
		priorityThreshold *int32
		pods              []*corev1.Pod
		wantUsagePerNode  map[string]resources.Requests
	}{
		"nil threshold, all pods counted": {
			priorityThreshold: nil,
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](-1),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](100),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("2"),
								},
							},
						}},
					},
				},
			},
			wantUsagePerNode: map[string]resources.Requests{
				"node1": {
					corev1.ResourceCPU:  3000,
					corev1.ResourcePods: 2,
				},
			},
		},
		"threshold 0, negative priority pods excluded": {
			priorityThreshold: ptr.To[int32](0),
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](-1),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](0),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("2"),
								},
							},
						}},
					},
				},
			},
			wantUsagePerNode: map[string]resources.Requests{
				"node1": {
					corev1.ResourceCPU:  2000,
					corev1.ResourcePods: 1,
				},
			},
		},
		"threshold 100, pods below 100 excluded": {
			priorityThreshold: ptr.To[int32](100),
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "low-pri", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](50),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						}},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "high-pri", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](100),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("2"),
								},
							},
						}},
					},
				},
			},
			wantUsagePerNode: map[string]resources.Requests{
				"node1": {
					corev1.ResourceCPU:  2000,
					corev1.ResourcePods: 1,
				},
			},
		},
		"pod with nil priority is counted when threshold is set": {
			priorityThreshold: ptr.To[int32](0),
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "no-pri", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						}},
					},
				},
			},
			wantUsagePerNode: map[string]resources.Requests{
				"node1": {
					corev1.ResourceCPU:  1000,
					corev1.ResourcePods: 1,
				},
			},
		},
		"all pods below threshold results in empty usage": {
			priorityThreshold: ptr.To[int32](0),
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
					Spec: corev1.PodSpec{
						NodeName: "node1",
						Priority: ptr.To[int32](-100),
						Containers: []corev1.Container{{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("4"),
								},
							},
						}},
					},
				},
			},
			wantUsagePerNode: map[string]resources.Requests{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, log := utiltesting.ContextWithLog(t)
			cache := &nonTasUsageCache{
				podUsage:          make(map[types.NamespacedName]podUsageValue),
				lock:              sync.RWMutex{},
				priorityThreshold: tc.priorityThreshold,
			}
			for _, pod := range tc.pods {
				cache.update(pod, log)
			}
			got := cache.usagePerNode()
			if diff := cmp.Diff(tc.wantUsagePerNode, got); diff != "" {
				t.Errorf("usagePerNode() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
