package mutators

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	discoblocksondatiov1 "github.com/ondat/discoblocks/api/v1"
	"github.com/ondat/discoblocks/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package
var podMutatorLog = logf.Log.WithName("mutators.PodMutator")

type PodMutator struct {
	Client  client.Client
	strict  bool
	decoder *admission.Decoder
}

//+kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,sideEffects=none,failurePolicy=fail,groups="",resources=pods,verbs=create,versions=v1,admissionReviewVersions=v1,name=mpod.kb.io

// Handle pod mutation
func (a *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := podMutatorLog.WithValues("name", req.Name, "namespace", req.Namespace)

	logger.Info("Handling...")
	defer logger.Info("Handled")

	pod := corev1.Pod{}
	if err := a.decoder.Decode(req, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("unable to decode request: %w", err))
	}

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	logger.Info("Fetch DiskConfigs...")

	diskConfigs := discoblocksondatiov1.DiskConfigList{}
	if err := a.Client.List(ctx, &diskConfigs, &client.ListOptions{
		Namespace: pod.Namespace,
	}); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to fetch configs: %w", err))
	}

	errorMode := func(code int32, reason string, err error) admission.Response {
		if a.strict {
			return admission.Errored(code, err)
		}

		return admission.Allowed(reason)
	}

	volumes := map[string]string{}
	for i := range diskConfigs.Items {
		if diskConfigs.Items[i].DeletionTimestamp != nil {
			continue
		}

		config := diskConfigs.Items[i]

		if !utils.IsContainsAll(pod.Labels, config.Spec.PodSelector) {
			continue
		}

		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels["discoblocks/metrics"] = config.Name

		//nolint:govet // logger is ok to shadowing
		logger := logger.WithValues("name", config.Name, "sc_name", config.Spec.StorageClassName)
		logger.Info("Attach volume to workload...")

		logger.Info("Fetch StorageClass...")

		sc := storagev1.StorageClass{}
		if err := a.Client.Get(ctx, types.NamespacedName{Name: config.Spec.StorageClassName}, &sc); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("StorageClass not found", "name", config.Spec.StorageClassName)
				return errorMode(http.StatusNotFound, "StorageClass not found: "+config.Spec.StorageClassName, err)
			}
			logger.Info("Unable to fetch StorageClass", "error", err.Error())
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to fetch StorageClass: %w", err))
		}
		logger = logger.WithValues("provisioner", sc.Provisioner)

		var pvc *corev1.PersistentVolumeClaim
		pvc, err := utils.NewPVC(&config, sc.Provisioner, logger)
		if err != nil {
			return errorMode(http.StatusInternalServerError, err.Error(), err)
		}

		logger.Info("Create PVC...")
		if err = a.Client.Create(ctx, pvc); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				logger.Info("Failed to create PVC", "error", err.Error())
				return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to create PVC: %w", err))
			}

			logger.Info("PVC already exists")
		}

		mountpoint := utils.RenderMountPoint(config.Spec.MountPointPattern, pvc.Name, 0)

		for name, mp := range volumes {
			if mp == mountpoint {
				logger.Info("Mount point already added", "exists", name, "actual", pvc.Name, "mountpoint", sc.Provisioner)
				return errorMode(http.StatusInternalServerError, "Unable to init a PVC", err)
			}
		}
		volumes[pvc.Name] = mountpoint

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: pvc.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.Name,
				},
			},
		})
	}

	if len(volumes) == 0 {
		return admission.Allowed("No sidecar injection")
	}

	pod.Spec.SchedulerName = "discoblocks-scheduler"

	logger.Info("Attach sidecars...")

	metricsSideCar, err := utils.RenderMetricsSidecar()
	if err != nil {
		logger.Error(err, "Metrics sidecar template invalid")
		return admission.Allowed("Metrics sidecar template invalid")
	}
	pod.Spec.Containers = append(pod.Spec.Containers, *metricsSideCar)

	logger.Info("Attach volume mounts...")

	for i := range pod.Spec.Containers {
		for name, mp := range volumes {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: mp,
			})
		}
	}

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "Unable to marshal pod")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to marshal pod: %w", err))
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// InjectDecoder sets decoder
func (a *PodMutator) InjectDecoder(d *admission.Decoder) error {
	a.decoder = d
	return nil
}

// NewPodMutator creates a new pod mutator
func NewPodMutator(kubeClient client.Client, strict bool) *PodMutator {
	return &PodMutator{
		Client: kubeClient,
		strict: strict,
	}
}
