/*
Copyright 2022.

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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	discoblocksondatiov1 "github.com/ondat/discoblocks/api/v1"
	"github.com/ondat/discoblocks/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// PVCReconciler reconciles a PVC object
type PVCReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Modify the Reconcile function to compare the state specified by
// the PVC object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *PVCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithName("PVCReconciler").WithValues("name", req.Name, "namespace", req.Name)

	lock, unlock := controllerSemaphore()
	if !lock {
		logger.Info("Another operation is on going, event needs to be resceduled")
		return ctrl.Result{Requeue: true}, nil
	}
	defer unlock()

	logger.Info("Reconciling...")
	defer logger.Info("Reconciled")

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	pvc := corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, req.NamespacedName, &pvc)
	switch {
	case err != nil && apierrors.IsNotFound(err):
		logger.Info("PVC not found")

		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("unable to fetch PVC: %w", err)
	}

	logger.Info("Fetch DiskConfig...")

	config := discoblocksondatiov1.DiskConfig{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: pvc.Namespace, Name: pvc.Labels["discoblocks"]}, &config); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("PVC not found")

			return ctrl.Result{}, nil
		}

		logger.Info("Unable to fetch PVC", "error", err.Error())
		return ctrl.Result{}, errors.New("unable to fetch PVC")
	}
	logger = logger.WithValues("dc_name", config.Name)

	if pvc.DeletionTimestamp != nil {
		if _, ok := config.Status.PersistentVolumeClaims[pvc.Name]; ok {
			logger.Info("Remove status")
			delete(config.Status.PersistentVolumeClaims, pvc.Name)
		}
	} else {
		if config.Status.PersistentVolumeClaims == nil {
			config.Status.PersistentVolumeClaims = map[string]corev1.PersistentVolumeClaimPhase{}
		}

		logger.Info("Add status", "phase", pvc.Status.Phase)
		config.Status.PersistentVolumeClaims[pvc.Name] = pvc.Status.Phase
	}

	// TODO update conditions

	logger.Info("Updating DiskConfig status...")

	if err := r.Client.Status().Update(ctx, &config); err != nil {
		logger.Info("Unable to update PVC status", "error", err.Error())
		return ctrl.Result{}, errors.New("unable to update PVC status")
	}

	logger.Info("Updated")

	return ctrl.Result{}, nil
}

//nolint:gocyclo // It is complex we know
func (r *PVCReconciler) MonitorVolumes() {
	logger := logf.Log.WithName("VolumeMonitor")

	logger.Info("Monitoring Volumes...")
	defer logger.Info("Monitor done")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute-time.Second)
	defer cancel()

	label, err := labels.NewRequirement("discoblocks", selection.Exists, nil)
	if err != nil {
		logger.Error(err, "Unable to parse Service label selector")
		return
	}
	endpointSelector := labels.NewSelector().Add(*label)

	endpoints := corev1.EndpointsList{}
	if err := r.Client.List(ctx, &endpoints, &client.ListOptions{
		LabelSelector: endpointSelector,
	}); err != nil {
		logger.Error(err, "Unable to fetch Services")
		return
	}

	discoblocks := map[types.NamespacedName][]string{}
	metrics := map[types.NamespacedName][]string{}
	for i := range endpoints.Items {
		// TODO detect not managed, finalizer like PVC if possible

		for _, ss := range endpoints.Items[i].Subsets {
			for _, ip := range ss.Addresses {
				podName := types.NamespacedName{Namespace: ip.TargetRef.Namespace, Name: ip.TargetRef.Name}

				if _, ok := discoblocks[podName]; !ok {
					discoblocks[podName] = []string{}
				}
				discoblocks[podName] = append(discoblocks[podName], endpoints.Items[i].Labels["discoblocks"])

				//nolint:govet // logger is ok to shadowing
				logger := logger.WithValues("pod_name", podName.String(), "ep_name", endpoints.Items[i].Name, "ip", ip.IP)

				// TODO https support would be nice
				req, err := http.NewRequest("GET", fmt.Sprintf("http://%s:9100/metrics", ip.IP), http.NoBody)
				if err != nil {
					logger.Error(err, "Request error")
					continue
				}

				// TODO shorter context would be great per request
				resp, err := http.DefaultClient.Do(req.WithContext(ctx))
				if err != nil {
					logger.Error(err, "Connection error")
					continue
				}

				rawBody, err := io.ReadAll(resp.Body)
				if err != nil {
					logger.Error(err, "Body read error")
					continue
				}
				if err = resp.Body.Close(); err != nil {
					logger.Error(err, "Body close error")
					continue
				}

				for _, line := range strings.Split(string(rawBody), "\n") {
					if strings.HasPrefix(line, "#") || !strings.Contains(line, "node_filesystem_avail_bytes") {
						continue
					}

					if _, ok := metrics[podName]; !ok {
						metrics[podName] = []string{}
					}
					metrics[podName] = append(metrics[podName], line)
				}
			}
		}
	}

	if len(metrics) == 0 {
		logger.Info("Metrics data not found")
		return
	}

	diskConfigCache := map[types.NamespacedName]discoblocksondatiov1.DiskConfig{}

	for podName, diskConfigNames := range discoblocks {
		//nolint:govet // logger is ok to shadowing
		logger := logger.WithValues("pod_name", podName.String())

		pod := corev1.Pod{}
		if err := r.Client.Get(ctx, podName, &pod); err != nil {
			logger.Error(err, "Failed to fetch pod error")
			continue
		}

		for _, diskConfigName := range diskConfigNames {
			diskConfigName := types.NamespacedName{Namespace: pod.Namespace, Name: diskConfigName}

			//nolint:govet // logger is ok to shadowing
			logger := logger.WithValues("dc_name", diskConfigName.String())

			config, ok := diskConfigCache[diskConfigName]
			if !ok {
				config = discoblocksondatiov1.DiskConfig{}
				if err := r.Client.Get(ctx, diskConfigName, &config); err != nil {
					logger.Error(err, "Failed to fetch DiskConfig error")
					continue
				}
				diskConfigCache[diskConfigName] = config
			}

			if config.Spec.Policy.Pause {
				logger.Info("Autoscaling paused")
				continue
			}

			for _, metric := range metrics[podName] {
				mf, err := utils.ParsePrometheusMetric(metric)
				if err != nil {
					logger.Error(err, "Failed to parse metrics")
					continue
				}

				if _, ok := mf["node_filesystem_avail_bytes"]; !ok {
					logger.Error(err, "Failed to find metric", "metric", metric)
					continue
				}

				mountpoint := ""
				for _, m := range mf["node_filesystem_avail_bytes"].Metric {
					for _, l := range m.Label {
						if *l.Name == "mountpoint" {
							mountpoint = *l.Value
						}
					}
				}
				if mountpoint == "" {
					logger.Error(err, "Failed to find mountpoint")
					continue
				}

				if mountpoint != utils.RenderMountPoint(config.Spec.MountPointPattern, config.Name, 0) {
					continue
				}

				var pvcName types.NamespacedName
				for i := range pod.Spec.Containers[0].VolumeMounts {
					vm := pod.Spec.Containers[0].VolumeMounts[i]

					if vm.MountPath == mountpoint {
						pvcName = types.NamespacedName{Namespace: pod.Namespace, Name: vm.Name}
						break
					}
				}
				if pvcName.Name == "" {
					logger.Error(err, "Volume not found")
					continue
				}

				// TODO maybe cache them and resize to the biggest in one step
				pvc := corev1.PersistentVolumeClaim{}
				if err = r.Client.Get(ctx, pvcName, &pvc); err != nil {
					logger.Error(err, "Failed to fetch PVC")
					continue
				}
				logger = logger.WithValues("pvc_name", pvc.Name)

				if !controllerutil.ContainsFinalizer(&pvc, utils.RenderFinalizer(config.Name)) {
					logger.Info("PVC not managed by", "config", pvc.Labels["discoblocks"])
					continue
				}

				// TODO abort if resizing by condition or pvc.Status.ResizeStatus

				available, err := utils.ParsePrometheusMetricValue(metric)
				if err != nil {
					logger.Error(err, "Metric is invalid")
					continue
				}

				maxCapacity, err := resource.ParseQuantity(config.Spec.Policy.MaximumCapacityOfDisk)
				if err != nil {
					logger.Error(err, "Max capacity is invalid")
					continue
				}

				const hundred = 100

				actualCapacity := pvc.Status.Capacity.Storage()
				treshold := actualCapacity.AsApproximateFloat64() * float64(config.Spec.Policy.UpscaleTriggerPercentage) / hundred

				logger.Info("Capacities", "available", available, "treshold", treshold, "actual", actualCapacity.AsApproximateFloat64(), "max", maxCapacity.AsApproximateFloat64())

				if treshold > actualCapacity.AsApproximateFloat64()-available {
					logger.Info("Disk size ok")
					continue
				}

				if actualCapacity.Cmp(maxCapacity) == 0 {
					logger.Info("New disk needed")
					continue
				}

				logger.Info("Resize needed")
				newCapacity, err := resource.ParseQuantity("1Gi")
				if err != nil {
					logger.Error(err, "Extend capacity is invalid")
					continue
				}
				newCapacity.Add(*actualCapacity)

				if maxCapacity.Cmp(newCapacity) == -1 {
					logger.Info("Set to max capacity")

					newCapacity = maxCapacity
				}

				logger.Info("Updating PVC...", "capacity", actualCapacity.AsApproximateFloat64())

				pvc.Spec.Resources.Requests[corev1.ResourceStorage] = newCapacity

				if err = r.Update(ctx, &pvc); err != nil {
					logger.Error(err, "Failed to update PVC")
					continue
				}
			}
		}
	}
}

type pvcEventFilter struct {
	logger logr.Logger
}

func (ef pvcEventFilter) Create(e event.CreateEvent) bool {
	newObj, ok := e.Object.(*corev1.PersistentVolumeClaim)
	if !ok {
		ef.logger.Error(errors.New("unsupported type"), "Unable to cast new object")
		return false
	}

	return controllerutil.ContainsFinalizer(newObj, utils.RenderFinalizer(newObj.Labels["discoblocks"]))
}

func (ef pvcEventFilter) Delete(_ event.DeleteEvent) bool {
	return false
}

func (ef pvcEventFilter) Update(e event.UpdateEvent) bool {
	newObj, ok := e.ObjectNew.(*corev1.PersistentVolumeClaim)
	if !ok {
		ef.logger.Error(errors.New("unsupported type"), "Unable to cast new object")
		return false
	}

	if !controllerutil.ContainsFinalizer(newObj, utils.RenderFinalizer(newObj.Labels["discoblocks"])) {
		return false
	}

	oldObj, ok := e.ObjectOld.(*corev1.PersistentVolumeClaim)
	if !ok {
		ef.logger.Error(errors.New("unsupported type"), "Unable to cast old object")
		return false
	}

	return oldObj.DeletionTimestamp != nil ||
		newObj.DeletionTimestamp != nil ||
		oldObj.Status.Phase != newObj.Status.Phase
}

func (ef pvcEventFilter) Generic(_ event.GenericEvent) bool {
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *PVCReconciler) SetupWithManager(mgr ctrl.Manager) (chan<- bool, error) {
	closeChan := make(chan bool)

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-closeChan:
				return
			case <-ticker.C:
				r.MonitorVolumes()
			}
		}
	}()

	return closeChan, ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolumeClaim{}).
		WithEventFilter(pvcEventFilter{logger: mgr.GetLogger().WithName("PVCReconciler")}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
		}).
		Complete(r)
}
