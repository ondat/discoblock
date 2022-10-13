package main

import (
	"fmt"
	"os"

	"github.com/valyala/fastjson"
)

func main() {}

//export IsStorageClassValid
func IsStorageClassValid() {
	json := []byte(os.Getenv("STORAGE_CLASS_JSON"))

	if !fastjson.Exists(json, "volumeBindingMode") || fastjson.GetString(json, "volumeBindingMode") != "WaitForFirstConsumer" {
		fmt.Fprint(os.Stderr, "only volumeBindingMode WaitForFirstConsumer is supported")
		fmt.Fprint(os.Stdout, false)
		return
	}

	if !fastjson.Exists(json, "allowVolumeExpansion") || !fastjson.GetBool(json, "allowVolumeExpansion") {
		fmt.Fprint(os.Stderr, "only allowVolumeExpansion true is supported")
		fmt.Fprint(os.Stdout, false)
		return
	}

	fmt.Fprint(os.Stdout, true)
}

//export GetPVCStub
func GetPVCStub() {
	fmt.Fprintf(os.Stdout, `{
	"apiVersion": "v1",
	"kind": "PersistentVolumeClaim",
	"metadata": {
		"name": "%s",
		"namespace": "%s"
	},
	"spec": {
		"storageClassName": "%s"
	}
}`,
		os.Getenv("PVC_NAME"), os.Getenv("PVC_NAMESACE"), os.Getenv("STORAGE_CLASS_NAME"))
}

//export GetCSIDriverNamespace
func GetCSIDriverNamespace() {
	fmt.Fprint(os.Stdout, "kube-system")
}

//export GetCSIDriverPodLabels
func GetCSIDriverPodLabels() {
	fmt.Fprint(os.Stdout, `{ "app": "ebs-csi-controller" }`)
}

//export GetPreMountCommand
func GetPreMountCommand() {
	fmt.Fprint(os.Stdout, `sleep infinity ; DEV=$(chroot /host nsenter --target 1 --mount mount | grep ${PV_NAME} | awk '{print $1}')`)
}

//export GetPreResizeCommand
func GetPreResizeCommand() {
	GetPreMountCommand()
}

//export IsFileSystemManaged
func IsFileSystemManaged() {
	fmt.Fprint(os.Stdout, false)
}

//export WaitForVolumeAttachmentMeta
func WaitForVolumeAttachmentMeta() {
	fmt.Fprint(os.Stdout, "devicePath")
}
