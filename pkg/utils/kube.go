package utils

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	discoblocksondatiov1 "github.com/ondat/discoblocks/api/v1"
	"github.com/ondat/discoblocks/pkg/drivers"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Used for Yaml indentation
const hostCommandPrefix = "\n          "

var hostCommandReplacePattern = regexp.MustCompile(`\n`)

const metricsServiceTemplate = `kind: Service
apiVersion: v1
metadata:
  name: "%s"
  namespace: "%s"
  annotations:
    prometheus.io/path: "/metrics"
    prometheus.io/scrape: "true"
    prometheus.io/port:   "9100"
spec:
  ports:
  - name: node-exporter
    protocol: TCP
    port: 9100
    targetPort: 9100
`

const metricsTeamplate = `name: discoblocks-metrics
image: bitnami/node-exporter:1.4.0
ports:
- containerPort: 9100
  protocol: TCP
command:
- /opt/bitnami/node-exporter/bin/node_exporter
- --collector.disable-defaults
- --collector.filesystem
- --collector.filesystem.mount-points-exclude="*"
- --collector.filesystem.fs-types-exclude="^(ext[2-4]|btrfs|xfs)$"
securityContext:
  privileged: false
`

const attachJobTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: "%s"
  namespace: "%s"
  labels:
    app: discoblocks
spec:
  template:
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchFields:
              - key: metadata.name
                operator: In
                values:
                - "%s"
      containers:
      - name: attach
        image: redhat/ubi8-micro@sha256:4f6f8db9a6dc949d9779a57c43954b251957bd4d019a37edbbde8ed5228fe90a
        command:
        - ls
        - /pvc
        volumeMounts:
        - mountPath: /pvc
          name: attach
          readOnly: true
      restartPolicy: Never
      volumes:
      - name: attach
        persistentVolumeClaim:
          claimName: "%s"
  backoffLimit: 0
  ttlSecondsAfterFinished: 86400
`

const hostJobTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: "%s"
  namespace: "%s"
  labels:
    app: discoblocks
spec:
  template:
    spec:
      hostPID: true
      nodeName: "%s"
      containers:
      - name: mount
        image: nixery.dev/shell/gawk/gnugrep/gnused/coreutils-full/cri-tools/docker-client
        env:
        - name: MOUNT_POINT
          value: "%s"
        - name: CONTAINER_IDS
          value: "%s"
        - name: PVC_NAME
          value: "%s"
        - name: PV_NAME
          value: "%s"
        - name: FS
          value: "%s"
        - name: VOLUME_ATTACHMENT_META
          value: "%s"
        command:
        - bash
        - -exc
        - |
          %s
        volumeMounts:
        - mountPath: /run/containerd/containerd.sock
          name: containerd-socket
          readOnly: true
        - mountPath: /var/run/docker.sock
          name: docker-socket
          readOnly: true
        - mountPath: /host
          name: host
        securityContext:
          privileged: true
      restartPolicy: Never
      volumes:
       - hostPath:
          path: /run/containerd/containerd.sock
         name: containerd-socket
       - hostPath:
          path: /var/run/docker.sock
         name: docker-socket
       - hostPath:
          path: /
         name: host
  backoffLimit: 3
  ttlSecondsAfterFinished: 86400
`

const (
	mountCommandTemplate = `%s
chroot /host nsenter --target 1 --mount mkdir -p /var/lib/kubelet/plugins/kubernetes.io/csi/pv/${PV_NAME}/globalmount &&
chroot /host nsenter --target 1 --mount mount ${DEV} /var/lib/kubelet/plugins/kubernetes.io/csi/pv/${PV_NAME}/globalmount &&
%s
echo ok`

	mknodMountTemplate = `DEV_MAJOR=$(chroot /host nsenter --target 1 --mount cat /proc/self/mountinfo | grep ${DEV} | awk '{print $3}'  | awk '{split($0,a,":"); print a[1]}') &&
DEV_MINOR=$(chroot /host nsenter --target 1 --mount cat /proc/self/mountinfo | grep ${DEV} | awk '{print $3}'  | awk '{split($0,a,":"); print a[2]}') &&
for CONTAINER_ID in ${CONTAINER_IDS}; do
	PID=$(docker inspect -f '{{.State.Pid}}' ${CONTAINER_ID} || crictl inspect --output go-template --template '{{.info.pid}}' ${CONTAINER_ID}) &&
	chroot /host nsenter --target ${PID} --mount mkdir -p /dev ${MOUNT_POINT} &&
	chroot /host nsenter --target ${PID} --pid --mount mknod ${DEV} b ${DEV_MAJOR} ${DEV_MINOR} &&
	chroot /host nsenter --target ${PID} --mount mount ${DEV} ${MOUNT_POINT}
done &&`

	bindMountTemplate = `chroot /host nsenter --target ${PID} --mount mount -o bind /var/lib/kubelet/plugins/kubernetes.io/csi/pv/${PV_NAME}/globalmount ${MOUNT_POINT} &&`
)

const resizeCommandTemplate = `%s
(:pvc:pvc
	([ "${FS}" = "ext3" ] && chroot /host nsenter --target 1 --mount resize2fs ${DEV}) ||
	([ "${FS}" = "ext4" ] && chroot /host nsenter --target 1 --mount resize2fs ${DEV}) ||
	([ "${FS}" = "xfs" ] && chroot /host nsenter --target 1 --mount xfs_growfs -d ${DEV}) ||
	([ "${FS}" = "btrfs" ] && chroot /host nsenter --target 1 --mount btrfs filesystem resize max ${DEV}) ||
	echo unsupported file-system $FS
) &&
echo ok`

// RenderMetricsService returns the metrics service
func RenderMetricsService(name, namespace string) (*corev1.Service, error) {
	service := corev1.Service{}
	if err := yaml.Unmarshal([]byte(fmt.Sprintf(metricsServiceTemplate, name, namespace)), &service); err != nil {
		return nil, fmt.Errorf("unable to unmarshal service: %w", err)
	}

	return &service, nil
}

// RenderMetricsSidecar returns the metrics sidecar
func RenderMetricsSidecar(privileged bool) (*corev1.Container, error) {
	sidecar := corev1.Container{}
	if err := yaml.Unmarshal([]byte(metricsTeamplate), &sidecar); err != nil {
		return nil, fmt.Errorf("unable to unmarshal container: %w", err)
	}

	sidecar.SecurityContext.Privileged = &privileged

	if privileged {
		sidecar.VolumeMounts = []corev1.VolumeMount{
			{
				Name:      "varlibkubelet",
				MountPath: "/var/lib/kubelet",
				ReadOnly:  true,
			},
		}
	}

	return &sidecar, nil
}

// RenderAttachJob returns the mount job executed on host
func RenderAttachJob(pvcName, namespace, nodeName string, owner metav1.OwnerReference) (*batchv1.Job, error) {
	jobName, err := RenderResourceName(true, fmt.Sprintf("%d", time.Now().UnixNano()), pvcName, namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to render resource name: %w", err)
	}

	template := fmt.Sprintf(attachJobTemplate, jobName, namespace, nodeName, pvcName)

	job := batchv1.Job{}
	if err := yaml.Unmarshal([]byte(template), &job); err != nil {
		println(template)
		return nil, fmt.Errorf("unable to unmarshal job: %w", err)
	}

	job.OwnerReferences = []metav1.OwnerReference{
		owner,
	}

	return &job, nil
}

// RenderMountJob returns the mount job executed on host
func RenderMountJob(pvcName, pvName, namespace, nodeName, fs, mountPoint string, containerIDs []string, preMountCommand string, hostPID bool, volumeMeta string, owner metav1.OwnerReference) (*batchv1.Job, error) {
	bindMount := mknodMountTemplate
	if hostPID {
		bindMount = bindMountTemplate
	}

	if preMountCommand != "" {
		preMountCommand += " && "
	}

	mountCommand := fmt.Sprintf(mountCommandTemplate, preMountCommand, bindMount)
	mountCommand = string(hostCommandReplacePattern.ReplaceAll([]byte(mountCommand), []byte(hostCommandPrefix)))

	jobName, err := RenderResourceName(true, fmt.Sprintf("%d", time.Now().UnixNano()), pvcName, namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to render resource name: %w", err)
	}

	template := fmt.Sprintf(hostJobTemplate, jobName, namespace, nodeName, mountPoint, strings.Join(containerIDs, " "), pvcName, pvName, fs, volumeMeta, mountCommand)

	job := batchv1.Job{}
	if err := yaml.Unmarshal([]byte(template), &job); err != nil {
		println(template)
		return nil, fmt.Errorf("unable to unmarshal job: %w", err)
	}

	job.OwnerReferences = []metav1.OwnerReference{
		owner,
	}

	return &job, nil
}

// RenderResizeJob returns the resize job executed on host
func RenderResizeJob(pvcName, pvName, namespace, nodeName, fs, preResizeCommand string, volumeMeta string, owner metav1.OwnerReference) (*batchv1.Job, error) {
	if preResizeCommand != "" {
		preResizeCommand += " && "
	}

	resizeCommand := fmt.Sprintf(resizeCommandTemplate, preResizeCommand)
	resizeCommand = string(hostCommandReplacePattern.ReplaceAll([]byte(resizeCommand), []byte(hostCommandPrefix)))

	jobName, err := RenderResourceName(true, fmt.Sprintf("%d", time.Now().UnixNano()), pvcName, namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to render resource name: %w", err)
	}

	template := fmt.Sprintf(hostJobTemplate, jobName, namespace, nodeName, "", "", pvcName, pvName, fs, volumeMeta, resizeCommand)

	job := batchv1.Job{}
	if err := yaml.Unmarshal([]byte(template), &job); err != nil {
		println(template)
		return nil, fmt.Errorf("unable to unmarshal job: %w", err)
	}

	job.OwnerReferences = []metav1.OwnerReference{
		owner,
	}

	return &job, nil
}

// NewPVC constructs a new PVC instance
func NewPVC(config *discoblocksondatiov1.DiskConfig, prefix string, driver *drivers.Driver) (*corev1.PersistentVolumeClaim, error) {
	pvcName, err := RenderResourceName(true, prefix, config.Name, config.Namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to calculate hash: %w", err)
	}

	pvc, err := driver.GetPVCStub(pvcName, config.Namespace, config.Spec.StorageClassName)
	if err != nil {
		return nil, fmt.Errorf("unable to init a PVC: %w", err)
	}

	pvc.Finalizers = []string{RenderFinalizer(config.Name)}

	pvc.Labels = map[string]string{
		"discoblocks": config.Name,
	}

	pvc.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceStorage: config.Spec.Capacity,
		},
	}

	pvc.Spec.AccessModes = config.Spec.AccessModes
	if len(pvc.Spec.AccessModes) == 0 {
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	return pvc, nil
}

// IsOwnedByDaemonSet detects is parent DaemonSet
func IsOwnedByDaemonSet(pod *corev1.Pod) bool {
	for i := range pod.OwnerReferences {
		if pod.OwnerReferences[i].Kind == "DaemonSet" && pod.OwnerReferences[i].APIVersion == appsv1.SchemeGroupVersion.String() {
			return true
		}
	}

	return false
}

// GetTargetNodeByAffinity tries to find node by affinity
func GetTargetNodeByAffinity(affinit *corev1.Affinity) string {
	if affinit == nil ||
		affinit.NodeAffinity == nil ||
		affinit.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}

	for _, term := range affinit.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, mf := range term.MatchFields {
			if mf.Key == "metadata.name" && mf.Operator == corev1.NodeSelectorOpIn && len(mf.Values) > 0 {
				// DeamonSet controller sets only one: https://sourcegraph.com/github.com/kubernetes/kubernetes@edd677694374fb8284b9ddd04caf0698eaf00de5/-/blob/pkg/controller/daemon/util/daemonset_util.go?L216
				return mf.Values[0]
			}
		}
	}

	return ""
}
