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

package tas

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
)

func TestFilterPodPriorityThreshold(t *testing.T) {
	cases := map[string]struct {
		priorityThreshold *int32
		pod               *corev1.Pod
		want              bool
	}{
		"nil threshold, pod with any priority is accepted": {
			priorityThreshold: nil,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					NodeName: "node1",
					Priority: ptr.To[int32](-100),
				},
			},
			want: true,
		},
		"threshold 0, negative priority pod is filtered": {
			priorityThreshold: ptr.To[int32](0),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					NodeName: "node1",
					Priority: ptr.To[int32](-1),
				},
			},
			want: false,
		},
		"threshold 0, zero priority pod is accepted": {
			priorityThreshold: ptr.To[int32](0),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					NodeName: "node1",
					Priority: ptr.To[int32](0),
				},
			},
			want: true,
		},
		"threshold 0, positive priority pod is accepted": {
			priorityThreshold: ptr.To[int32](0),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					NodeName: "node1",
					Priority: ptr.To[int32](100),
				},
			},
			want: true,
		},
		"threshold 0, nil priority pod is accepted": {
			priorityThreshold: ptr.To[int32](0),
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					NodeName: "node1",
				},
			},
			want: true,
		},
		"unscheduled pod is always filtered": {
			priorityThreshold: nil,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "ns"},
				Spec: corev1.PodSpec{
					Priority: ptr.To[int32](100),
				},
			},
			want: false,
		},
		"TAS pod is always filtered": {
			priorityThreshold: nil,
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: "ns",
					Labels: map[string]string{
						kueue.TASLabel: "true",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: "node1",
					Priority: ptr.To[int32](100),
				},
			},
			want: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := &NonTasUsageReconciler{
				priorityThreshold: tc.priorityThreshold,
			}
			got := r.filterPod(tc.pod)
			if got != tc.want {
				t.Errorf("filterPod() = %v, want %v", got, tc.want)
			}
		})
	}
}
