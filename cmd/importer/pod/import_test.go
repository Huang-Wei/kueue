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

package pod

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/cmd/importer/cache"
	"sigs.k8s.io/kueue/cmd/importer/mapping"
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta2"
	testingpod "sigs.k8s.io/kueue/pkg/util/testingjobs/pod"
	"sigs.k8s.io/kueue/pkg/workload"
)

func TestImportNamespace(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	basePodWrapper := testingpod.MakePod("pod", testingNamespace).
		UID("pod").
		Label(testingQueueLabel, "q1").
		Image("img", nil).
		Request(corev1.ResourceCPU, "1")

	baseWlWrapper := utiltestingapi.MakeWorkload("pod-pod-b17ab", testingNamespace).
		ControllerReference(corev1.SchemeGroupVersion.WithKind("Pod"), "pod", "pod").
		Label(controllerconstants.JobUIDLabel, "pod").
		Finalizers(kueue.ResourceInUseFinalizerName).
		Queue("lq1").
		PodSets(*utiltestingapi.MakePodSet(kueue.DefaultPodSetName, 1).
			Image("img").
			Request(corev1.ResourceCPU, "1").
			PodIndexLabel(ptr.To(kueue.PodGroupPodIndexLabel)).
			Obj()).
		ReserveQuotaAt(utiltestingapi.MakeAdmission("cq1").
			PodSets(utiltestingapi.MakePodSetAssignment(kueue.DefaultPodSetName).
				Assignment(corev1.ResourceCPU, "f1", "1").
				Obj()).
			Obj(), now).
		Condition(metav1.Condition{
			Type:    kueue.WorkloadQuotaReserved,
			Status:  metav1.ConditionTrue,
			Reason:  "Imported",
			Message: "Imported into ClusterQueue cq1",
		}).
		Condition(metav1.Condition{
			Type:    kueue.WorkloadAdmitted,
			Status:  metav1.ConditionTrue,
			Reason:  "Imported",
			Message: "Imported into ClusterQueue cq1",
		})

	baseLocalQueue := utiltestingapi.MakeLocalQueue("lq1", testingNamespace).ClusterQueue("cq1")
	baseClusterQueue := utiltestingapi.MakeClusterQueue("cq1").
		ResourceGroup(
			*utiltestingapi.MakeFlavorQuotas("f1").Resource(corev1.ResourceCPU, "1", "0").Obj())

	podCmpOpts := cmp.Options{
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion"),
	}

	wlCmpOpts := cmp.Options{
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(metav1.ObjectMeta{}, "ResourceVersion"),
		cmpopts.IgnoreFields(metav1.Condition{}, "ObservedGeneration", "LastTransitionTime"),
	}

	cases := map[string]struct {
		pods          []corev1.Pod
		clusterQueues []kueue.ClusterQueue
		localQueues   []kueue.LocalQueue
		mapping       mapping.Rules
		addLabels     map[string]string

		wantPods      []corev1.Pod
		wantWorkloads []kueue.Workload
		wantError     error
	}{

		"create one": {
			pods: []corev1.Pod{
				*basePodWrapper.Clone().Obj(),
			},
			mapping: mapping.Rules{
				mapping.Rule{
					Match: mapping.Match{
						PriorityClassName: "",
						Labels: map[string]string{
							testingQueueLabel: "q1",
						},
					},
					ToLocalQueue: "lq1",
				},
			},
			localQueues: []kueue.LocalQueue{
				*baseLocalQueue.Obj(),
			},
			clusterQueues: []kueue.ClusterQueue{
				*baseClusterQueue.Obj(),
			},

			wantPods: []corev1.Pod{
				*basePodWrapper.Clone().
					Label(controllerconstants.QueueLabel, "lq1").
					ManagedByKueueLabel().
					Obj(),
			},

			wantWorkloads: []kueue.Workload{
				*baseWlWrapper.Clone().Obj(),
			},
		},
		"create one, add labels": {
			pods: []corev1.Pod{
				*basePodWrapper.Clone().Obj(),
			},
			mapping: mapping.Rules{
				mapping.Rule{
					Match: mapping.Match{
						PriorityClassName: "",
						Labels: map[string]string{
							testingQueueLabel: "q1",
						},
					},
					ToLocalQueue: "lq1",
				},
			},
			localQueues: []kueue.LocalQueue{
				*baseLocalQueue.Obj(),
			},
			clusterQueues: []kueue.ClusterQueue{
				*baseClusterQueue.Obj(),
			},
			addLabels: map[string]string{
				"new.lbl": "val",
			},

			wantPods: []corev1.Pod{
				*basePodWrapper.Clone().
					Label(controllerconstants.QueueLabel, "lq1").
					ManagedByKueueLabel().
					Label("new.lbl", "val").
					Obj(),
			},

			wantWorkloads: []kueue.Workload{
				*baseWlWrapper.Clone().
					Label("new.lbl", "val").
					Obj(),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			podsList := corev1.PodList{Items: tc.pods}
			cqList := kueue.ClusterQueueList{Items: tc.clusterQueues}
			lqList := kueue.LocalQueueList{Items: tc.localQueues}

			builder := utiltesting.NewClientBuilder().
				WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: utiltesting.TreatSSAAsStrategicMerge}).WithStatusSubresource(&kueue.Workload{}).
				WithLists(&podsList, &cqList, &lqList)

			client := builder.Build()
			ctx, _ := utiltesting.ContextWithLog(t)

			mpc, _ := cache.Load(ctx, client, []string{testingNamespace}, tc.mapping, tc.addLabels)
			gotErr := Import(ctx, client, mpc, 8)

			if diff := cmp.Diff(tc.wantError, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected error (-want/+got)\n%s", diff)
			}

			err := client.List(ctx, &podsList)
			if err != nil {
				t.Errorf("Unexpected list pod error: %s", err)
			}
			if diff := cmp.Diff(tc.wantPods, podsList.Items, podCmpOpts...); diff != "" {
				t.Errorf("Unexpected pods (-want/+got)\n%s", diff)
			}

			wlList := kueue.WorkloadList{}
			err = client.List(ctx, &wlList)
			if err != nil {
				t.Errorf("Unexpected list workloads error: %s", err)
			}
			if diff := cmp.Diff(tc.wantWorkloads, wlList.Items, wlCmpOpts...); diff != "" {
				t.Errorf("Unexpected workloads (-want/+got)\n%s", diff)
			}
		})
	}
}

func TestRetryOnAPIError(t *testing.T) {
	gr := schema.GroupResource{Group: kueue.GroupVersion.Group, Resource: "workloads"}
	conflict := apierrors.NewConflict(gr, "wl", errors.New("the object has been modified"))
	serverTimeout := apierrors.NewServerTimeout(gr, "create", 0)
	notFound := apierrors.NewNotFound(gr, "wl")

	cases := map[string]struct {
		// errs[i] is returned by the i-th call to do, the last one is repeated.
		errs        []error
		reloadErr   error
		cancelCtx   bool
		wantErr     error
		wantCalls   int
		wantReloads int
	}{
		"success": {
			errs:      []error{nil},
			wantCalls: 1,
		},
		"a non retryable error is returned as is": {
			errs:      []error{notFound},
			wantErr:   notFound,
			wantCalls: 1,
		},
		"a transient conflict is retried after a reload": {
			errs:        []error{conflict, nil},
			wantCalls:   2,
			wantReloads: 1,
		},
		"a permanent conflict is given up on": {
			errs:        []error{conflict},
			wantErr:     conflict,
			wantCalls:   retryLimit + 1,
			wantReloads: retryLimit,
		},
		"an error suggesting a client delay is given up on, without reloading": {
			errs:      []error{serverTimeout},
			wantErr:   serverTimeout,
			wantCalls: retryLimit + 1,
		},
		// A failing reload is retried too, so do and reload alternate until the
		// shared retry budget runs out.
		"a failing reload is given up on": {
			errs:        []error{conflict},
			reloadErr:   serverTimeout,
			wantErr:     serverTimeout,
			wantCalls:   3,
			wantReloads: 3,
		},
		"a canceled context interrupts the retries": {
			errs:      []error{conflict},
			cancelCtx: true,
			wantErr:   context.Canceled,
			wantCalls: 1,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, log := utiltesting.ContextWithLog(t)
			if tc.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			var calls, reloads int
			gotErr := retryOnAPIError(ctx, log,
				func() error {
					reloads++
					return tc.reloadErr
				},
				func() error {
					calls++
					return tc.errs[min(calls, len(tc.errs))-1]
				},
			)

			if diff := cmp.Diff(tc.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected error (-want/+got)\n%s", diff)
			}
			if calls != tc.wantCalls {
				t.Errorf("Called do %d times, want %d", calls, tc.wantCalls)
			}
			if reloads != tc.wantReloads {
				t.Errorf("Called reload %d times, want %d", reloads, tc.wantReloads)
			}
		})
	}
}

// The admission apply carries the Workload's resourceVersion, so it conflicts
// whenever the Workload is modified between its creation and its admission, as
// kueue-controller-manager does when it reconciles it. Replaying the apply from
// the stale copy keeps conflicting, so the retries have to reload the Workload,
// and give up if it still does not converge: an import worker that retries a
// Pod forever never picks up another one, and once all the workers are stuck
// the importer hangs instead of terminating.
func TestImportRetriesConflictingAdmission(t *testing.T) {
	podWrapper := testingpod.MakePod("pod", testingNamespace).
		UID("pod").
		Label(testingQueueLabel, "q1").
		Image("img", nil).
		Request(corev1.ResourceCPU, "1")

	cases := map[string]struct {
		conflicts int

		wantAdmitted bool
		wantConflict bool
		wantPatches  int
	}{
		"a transient conflict is retried": {
			conflicts:    1,
			wantAdmitted: true,
			wantPatches:  2,
		},
		"a permanent conflict fails the Pod, but the import still terminates": {
			conflicts:    math.MaxInt,
			wantConflict: true,
			wantPatches:  retryLimit + 1,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			podsList := corev1.PodList{Items: []corev1.Pod{*podWrapper.Clone().Obj()}}
			lqList := kueue.LocalQueueList{Items: []kueue.LocalQueue{
				*utiltestingapi.MakeLocalQueue("lq1", testingNamespace).ClusterQueue("cq1").Obj(),
			}}
			cqList := kueue.ClusterQueueList{Items: []kueue.ClusterQueue{
				*utiltestingapi.MakeClusterQueue("cq1").ResourceGroup(
					*utiltestingapi.MakeFlavorQuotas("f1").Resource(corev1.ResourceCPU, "1", "0").Obj()).Obj(),
			}}

			patches := 0
			client := utiltesting.NewClientBuilder().
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, clnt client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						patches++
						if patches <= tc.conflicts {
							return apierrors.NewConflict(
								kueue.GroupVersion.WithResource("workloads").GroupResource(),
								obj.GetName(), errors.New("the object has been modified"))
						}
						return utiltesting.TreatSSAAsStrategicMerge(ctx, clnt, subResourceName, obj, patch, opts...)
					},
				}).
				WithStatusSubresource(&kueue.Workload{}).
				WithLists(&podsList, &cqList, &lqList).
				Build()
			ctx, _ := utiltesting.ContextWithLog(t)

			mpc, err := cache.Load(ctx, client, []string{testingNamespace}, mapping.Rules{
				mapping.Rule{
					Match:        mapping.Match{Labels: map[string]string{testingQueueLabel: "q1"}},
					ToLocalQueue: "lq1",
				},
			}, nil)
			if err != nil {
				t.Fatalf("Unexpected cache load error: %s", err)
			}

			errCh := make(chan error, 1)
			go func() { errCh <- Import(ctx, client, mpc, 1) }()

			var gotErr error
			select {
			case gotErr = <-errCh:
			case <-time.After(time.Minute):
				t.Fatalf("Import did not terminate, %d admission patches attempted", patches)
			}

			if gotConflict := apierrors.IsConflict(gotErr); gotConflict != tc.wantConflict {
				t.Errorf("Import returned %v, want a conflict: %v", gotErr, tc.wantConflict)
			}
			if patches != tc.wantPatches {
				t.Errorf("Attempted %d admission patches, want %d", patches, tc.wantPatches)
			}

			wlList := kueue.WorkloadList{}
			if err := client.List(ctx, &wlList); err != nil {
				t.Fatalf("Unexpected list workloads error: %s", err)
			}
			if len(wlList.Items) != 1 {
				t.Fatalf("Got %d workloads, want 1", len(wlList.Items))
			}
			if gotAdmitted := workload.IsAdmitted(&wlList.Items[0]); gotAdmitted != tc.wantAdmitted {
				t.Errorf("Workload admitted is %v, want %v", gotAdmitted, tc.wantAdmitted)
			}
		})
	}
}
