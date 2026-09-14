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
	"fmt"
	"maps"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/cmd/importer/cache"
	"sigs.k8s.io/kueue/pkg/constants"
	controllerconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobs/pod"
	"sigs.k8s.io/kueue/pkg/workload"
)

var realClock = clock.RealClock{}

func Import(ctx context.Context, c client.Client, importCache *cache.ImportCache, jobs uint) error {
	ch := make(chan corev1.Pod)
	listErrCh := make(chan error, 1)
	go func() {
		listErrCh <- ListPods(ctx, c, importCache.Namespaces, ch)
	}()
	summary := ProcessConcurrently(ch, jobs, func(p *corev1.Pod) (bool, error) {
		log := ctrl.LoggerFrom(ctx).WithValues("pod", klog.KObj(p))
		log.V(3).Info("Importing")

		lq, skip, err := importCache.LocalQueue(p)
		if skip || err != nil {
			return skip, err
		}

		oldLq, found := p.Labels[controllerconstants.QueueLabel]
		if !found {
			if err := addLabels(ctx, c, p, lq.Name, importCache.AddLabels); err != nil {
				return false, fmt.Errorf("cannot add queue label: %w", err)
			}
		} else if oldLq != lq.Name {
			return false, fmt.Errorf("another local queue name is set %q expecting %q", oldLq, lq.Name)
		}

		kp := pod.FromObject(p)
		// Note: the recorder is not used for single pods, we can just pass nil for now.
		wl, err := kp.ConstructComposableWorkload(ctx, c, nil, nil)
		if err != nil {
			return false, fmt.Errorf("construct workload: %w", err)
		}

		maps.Copy(wl.Labels, importCache.AddLabels)

		if pc, found := importCache.PriorityClasses[p.Spec.PriorityClassName]; found {
			wl.Spec.PriorityClassRef = kueue.NewPodPriorityClassRef(pc.Name)
			wl.Spec.Priority = &pc.Value
		}

		if err := createWorkload(ctx, c, wl); err != nil {
			return false, fmt.Errorf("creating workload: %w", err)
		}

		if err := admitWorkload(ctx, c, wl, importCache.ClusterQueues[string(lq.Spec.ClusterQueue)]); err != nil {
			return false, err
		}
		log.V(2).Info("Successfully imported", "pod", klog.KObj(p), "workload", klog.KObj(wl))
		return false, nil
	})

	log := ctrl.LoggerFrom(ctx)
	log.Info("Import done", "checked", summary.TotalPods, "skipped", summary.SkippedPods, "failed", summary.FailedPods)
	for e, pods := range summary.ErrorsForPods {
		log.Info("Import failed for Pods", "err", e, "occurrences", len(pods), "observedFirstIn", pods[0])
	}
	// A listing failure leaves Pods unimported, so it must not be reported as success.
	return errors.Join(append(summary.Errors, <-listErrCh)...)
}

const (
	// retryLimit caps the number of retries done for a single API call. The
	// retries have to be bounded: an import worker that keeps retrying a Pod
	// never picks up another one, and once every worker is stuck the importer
	// hangs instead of terminating.
	retryLimit = 5

	// retryDelay is the base of the linear backoff used when the API server
	// does not suggest a delay itself, as is the case for conflicts.
	retryDelay = 100 * time.Millisecond
)

func checkError(err error) (retry, reload bool, timeout time.Duration) {
	retrySeconds, retry := apierrors.SuggestsClientDelay(err)
	if retry {
		return true, false, time.Duration(retrySeconds) * time.Second
	}

	if apierrors.IsConflict(err) {
		return true, true, 0
	}
	return false, false, 0
}

// retryOnAPIError calls do until it succeeds or fails with an error that is not
// worth retrying, giving up after retryLimit retries.
//
// reload is called before replaying an operation that failed with a conflict and
// should refresh the caller's copy of the object. An update or an apply built
// from a stale copy carries a stale resourceVersion, so the API server rejects
// the replay with the very same conflict; without reloading, the retries can
// never converge.
func retryOnAPIError(ctx context.Context, log logr.Logger, reload func() error, do func() error) error {
	err := do()
	for attempt := 1; ; attempt++ {
		retry, needsReload, timeout := checkError(err)
		if !retry {
			return err
		}
		if attempt > retryLimit {
			return fmt.Errorf("gave up after %d retries: %w", retryLimit, err)
		}
		if timeout <= 0 {
			timeout = time.Duration(attempt) * retryDelay
		}
		log.V(2).Info("Retrying after API error", "attempt", attempt, "after", timeout, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(timeout):
		}
		if needsReload && reload != nil {
			if err = reload(); err != nil {
				continue
			}
		}
		err = do()
	}
}

func addLabels(ctx context.Context, c client.Client, p *corev1.Pod, queue string, addLabels map[string]string) error {
	setLabels := func() {
		if p.Labels == nil {
			p.Labels = make(map[string]string, 2+len(addLabels))
		}
		p.Labels[controllerconstants.QueueLabel] = queue
		p.Labels[constants.ManagedByKueueLabelKey] = constants.ManagedByKueueLabelValue
		maps.Copy(p.Labels, addLabels)
	}

	setLabels()
	return retryOnAPIError(ctx, ctrl.LoggerFrom(ctx),
		func() error {
			if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				return err
			}
			setLabels()
			return nil
		},
		func() error { return c.Update(ctx, p) },
	)
}

func createWorkload(ctx context.Context, c client.Client, wl *kueue.Workload) error {
	return retryOnAPIError(ctx, ctrl.LoggerFrom(ctx), nil, func() error {
		err := c.Create(ctx, wl)
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	})
}

func admitWorkload(ctx context.Context, c client.Client, wl *kueue.Workload, cq *kueue.ClusterQueue) error {
	update := func(wl *kueue.Workload) (bool, error) {
		// make its admission and update its status
		info := workload.NewInfo(wl)

		admission := kueue.Admission{
			ClusterQueue: kueue.ClusterQueueReference(cq.Name),
			PodSetAssignments: []kueue.PodSetAssignment{
				{
					Name:          info.TotalRequests[0].Name,
					Flavors:       make(map[corev1.ResourceName]kueue.ResourceFlavorReference),
					ResourceUsage: info.TotalRequests[0].Requests.ToResourceList(),
					Count:         ptr.To[int32](1),
				},
			},
		}
		flv := cq.Spec.ResourceGroups[0].Flavors[0].Name
		for r := range info.TotalRequests[0].Requests {
			admission.PodSetAssignments[0].Flavors[r] = flv
		}

		wl.Status.Admission = &admission
		reservedCond := metav1.Condition{
			Type:    kueue.WorkloadQuotaReserved,
			Status:  metav1.ConditionTrue,
			Reason:  "Imported",
			Message: fmt.Sprintf("Imported into ClusterQueue %s", cq.Name),
		}
		apimeta.SetStatusCondition(&wl.Status.Conditions, reservedCond)
		admittedCond := metav1.Condition{
			Type:    kueue.WorkloadAdmitted,
			Status:  metav1.ConditionTrue,
			Reason:  "Imported",
			Message: fmt.Sprintf("Imported into ClusterQueue %s", cq.Name),
		}
		apimeta.SetStatusCondition(&wl.Status.Conditions, admittedCond)
		return true, nil
	}

	return retryOnAPIError(ctx, ctrl.LoggerFrom(ctx),
		// The apply carries the Workload's resourceVersion, so it keeps
		// conflicting until we pick up the version written by whoever modified
		// the Workload after we created it, typically kueue-controller-manager
		// reconciling it.
		func() error { return c.Get(ctx, client.ObjectKeyFromObject(wl), wl) },
		func() error {
			return workload.PatchAdmissionStatus(ctx, c, wl, realClock, update, workload.WithForceApply())
		},
	)
}
