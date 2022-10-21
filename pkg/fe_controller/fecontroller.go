/*
Copyright 2021-present, StarRocks Inc.

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

package fe_controller

import (
	"context"
	"errors"
	srapi "github.com/StarRocks/starrocks-kubernetes-operator/api/v1alpha1"
	rutils "github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/resource_utils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type FeController struct {
	k8sclient client.Client
	k8sReader client.Reader
}

func New(k8sclient client.Client, k8sReader client.Reader) *FeController {
	return &FeController{
		k8sclient: k8sclient,
		k8sReader: k8sReader,
	}
}

//Sync starRocksCluster spec to fe statefulset and service.
func (fc *FeController) Sync(ctx context.Context, src *srapi.StarRocksCluster) error {
	if src.Spec.StarRocksFeSpec == nil {
		klog.Info("FeController Sync", "the fe component is not needed", "namespace", src.Namespace, "starrocks cluster name", src.Name)
		return nil
	}

	//generate new fe service.
	svc := rutils.BuildService(src, rutils.FeService)
	fs := srapi.StarRocksFeStatus{ServiceName: svc.Name}
	src.Status.StarRocksFeStatus = &fs
	//create or update fe service, update the status of fe on src.
	if err := k8sutils.CreateOrUpdateService(ctx, fc.k8sclient, &svc); err != nil {
		klog.Error("FeController Sync", "create or update service namespace", svc.Namespace, "name", svc.Name)
		return err
	}
	feFinalizers := []string{srapi.FE_SERVICE_FINALIZER}
	defer func() {
		rutils.MergeSlices(src.Finalizers, feFinalizers)
	}()

	//create fe statefulset.
	st := rutils.NewStatefulset(fc.buildStatefulSetParams(src))
	var est appv1.StatefulSet
	err := fc.k8sclient.Get(ctx, types.NamespacedName{Namespace: st.Namespace, Name: st.Name}, &est)
	if err != nil && apierrors.IsNotFound(err) {
		fs.ResourceNames = append(fs.ResourceNames, st.Name)
		feFinalizers = append(feFinalizers, srapi.FE_STATEFULSET_FINALIZER)
		return k8sutils.CreateClientObject(ctx, fc.k8sclient, &st)
	} else if err != nil {
		return err
	}

	//if the spec is not change, update the status of fe on src.
	if rutils.StatefulSetDeepEqual(&st, est) {
		fs.ResourceNames = rutils.MergeSlices(fs.ResourceNames, []string{st.Name})
		if err := fc.updateFeStatus(&fs, st); err != nil {
			return err
		}
		//no update
		return nil
	}

	//fe spec changed update the statefulset.
	rutils.MergeStatefulSets(&st, est)
	if err := k8sutils.UpdateClientObject(ctx, fc.k8sclient, &st); err != nil {
		return err
	}

	return fc.updateFeStatus(&fs, st)
}

//buildStatefulSetParams generate the params of construct the statefulset.
func (fc *FeController) buildStatefulSetParams(src *srapi.StarRocksCluster) rutils.StatefulSetParams {
	feSpec := src.Spec.StarRocksFeSpec
	var pvcs []corev1.PersistentVolumeClaim
	for _, vm := range feSpec.StorageVolumes {
		pvcs = append(pvcs, corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: vm.Name},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{
					corev1.ReadWriteOnce,
				},
				StorageClassName: vm.StorageClassName,
			},
		})
	}

	stname := src.Name + "-" + srapi.DEFAULT_FE
	if feSpec.Name != "" {
		stname = feSpec.Name
	}

	var labels rutils.Labels
	labels[srapi.OwnerReference] = src.Name
	labels[srapi.ComponentLabelKey] = srapi.DEFAULT_FE
	labels.AddLabel(src.Labels)
	or := metav1.OwnerReference{
		UID:        src.UID,
		Kind:       src.Kind,
		APIVersion: src.APIVersion,
		Name:       src.Name,
	}

	return rutils.StatefulSetParams{
		Name:                 stname,
		Namespace:            src.Namespace,
		VolumeClaimTemplates: pvcs,
		ServiceName:          fc.getFeServiceName(src),
		PodTemplateSpec:      fc.buildPodTemplate(src),
		Labels:               labels,
		Selector:             labels,
		OwnerReferences:      []metav1.OwnerReference{or},
	}
}

//UpdateFeStatus update the starrockscluster fe status.
func (fc *FeController) updateFeStatus(fs *srapi.StarRocksFeStatus, st appv1.StatefulSet) error {
	var podList corev1.PodList
	if err := fc.k8sReader.List(context.Background(), &podList, client.InNamespace(st.Namespace), client.MatchingLabels(st.Spec.Selector.MatchLabels)); err != nil {
		return err
	}

	var creatings, runnings, faileds []string
	var podmap map[string]corev1.Pod
	//get all pod status that controlled by st.
	for _, pod := range podList.Items {
		//TODO: test
		podmap[pod.Name] = pod
		if pod.Status.Phase == corev1.PodPending {
			creatings = append(creatings, pod.Name)
		} else if pod.Status.Phase == corev1.PodRunning {
			runnings = append(runnings, pod.Name)
		} else {
			faileds = append(faileds, pod.Name)
		}
	}

	fs.Phase = srapi.ComponentReconciling
	if len(runnings) == int(*st.Spec.Replicas) {
		fs.Phase = srapi.ComponentRunning
	} else if len(faileds) != 0 {
		fs.Phase = srapi.ComponentFailed
		fs.Reason = podmap[faileds[0]].Status.Reason
	} else if len(creatings) != 0 {
		fs.Phase = srapi.ComponentPending
		fs.Reason = podmap[creatings[0]].Status.Reason
	}

	return nil
}

//buildPodTemplate construct the podTemplate for deploy fe.
func (fc *FeController) buildPodTemplate(src *srapi.StarRocksCluster) corev1.PodTemplateSpec {
	metaname := src.Name + "-" + srapi.DEFAULT_FE
	labels := src.Labels
	feSpec := src.Spec.StarRocksFeSpec
	labels[srapi.OwnerReference] = fc.getFeServiceName(src)

	vols := []corev1.Volume{
		//TODO：cancel the configmap for temporary.
		/*{
			Name: srapi.DEFAULT_FE_CONFIG_NAME,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: srapi.DEFAULT_FE_CONFIG_NAME,
					},
				},
			},
		},*/
		{
			Name: srapi.DEFAULT_EMPTDIR_NAME,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	var volMounts []corev1.VolumeMount
	for _, vm := range feSpec.StorageVolumes {
		volMounts = append(volMounts, corev1.VolumeMount{
			Name:      vm.Name,
			MountPath: vm.MountPath,
		}, corev1.VolumeMount{
			Name:      srapi.INITIAL_VOLUME_PATH_NAME,
			MountPath: srapi.INITIAL_VOLUME_PATH,
		})
	}

	opContainers := []corev1.Container{
		{
			Name:  srapi.DEFAULT_FE,
			Image: feSpec.Image,
			//TODO: add start command
			Command: []string{"fe/bin/start_fe.sh"},
			//TODO: add args
			Args: []string{"--daemon"},
			Ports: []corev1.ContainerPort{{
				Name:          "http_port",
				ContainerPort: 8030,
				Protocol:      corev1.ProtocolTCP,
			}, {
				Name:          "rpc_port",
				ContainerPort: 9020,
				Protocol:      corev1.ProtocolTCP,
			}, {
				Name:          "query_port",
				ContainerPort: 9030,
				Protocol:      corev1.ProtocolTCP,
			},
			},
			Env: []corev1.EnvVar{
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
					},
				}, {
					Name: "POD_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
					},
				}, {
					Name:  srapi.COMPONENT_NAME,
					Value: srapi.DEFAULT_FE,
				}, {
					Name:  srapi.SERVICE_NAME,
					Value: fc.getFeServiceName(src),
				}, {
					Name: "POD_IP",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
					},
				}, {
					Name: "HOST_IP",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
					},
				},
			},

			Resources:    feSpec.ResourceRequirements,
			VolumeMounts: volMounts,
			//TODO: LivenessProbe,ReadinessProbe
			ImagePullPolicy: corev1.PullIfNotPresent,
			//TODO: cancel the startProbe for temporary.
			/*StartupProbe: &corev1.Probe{
				InitialDelaySeconds: 5,
				FailureThreshold:    120,
				PeriodSeconds:       5,
				ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 9010,
				}}},
			},*/
		},
	}

	iniContainers := []corev1.Container{
		{
			//TODO: set the start command.
			Command: []string{"/opt/starrocks/init-container-entrypoint.sh"},
			Name:    "init-prepare",
			Image:   feSpec.Image,
			Env: []corev1.EnvVar{
				{
					Name:  "FE_SERVICE_NAME",
					Value: fc.getFeServiceName(src) + "." + src.Namespace,
				},
				{
					Name:  "LEADER_FILE",
					Value: "/pod-data/leader",
				},
			},
		},
	}
	podSpec := corev1.PodSpec{
		InitContainers:                iniContainers,
		Volumes:                       vols,
		Containers:                    opContainers,
		ServiceAccountName:            src.Spec.ServiceAccount,
		TerminationGracePeriodSeconds: int64ptr(int64(30)),
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:        metaname,
			Namespace:   src.Namespace,
			Labels:      labels,
			Annotations: src.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: src.APIVersion,
					Kind:       src.Kind,
					Name:       src.Name,
				},
			},
		},
		Spec: podSpec,
	}
}

//ClearResources clear resource about fe.
func (fc *FeController) ClearResources(ctx context.Context, src *srapi.StarRocksCluster) (bool, error) {
	//if the starrocks is not have fe.
	if src.Status.StarRocksFeStatus == nil {
		return true, nil
	}

	fmap := map[string]bool{}
	count := 0
	defer func() {
		finalizers := []string{}
		for _, f := range src.Finalizers {
			if _, ok := fmap[f]; !ok {
				finalizers = append(finalizers, f)
			}
		}
		src.Finalizers = finalizers
	}()

	for _, name := range src.Status.StarRocksFeStatus.ResourceNames {
		if _, err := k8sutils.DeleteClientObject(ctx, fc.k8sclient, src.Namespace, name); err != nil {
			return false, errors.New("fe delete statefulset" + err.Error())
		}
	}

	if count == len(src.Status.StarRocksFeStatus.ResourceNames) {
		fmap[srapi.FE_STATEFULSET_FINALIZER] = true
	}

	if _, ok := fmap[srapi.FE_STATEFULSET_FINALIZER]; !ok {
		return k8sutils.DeleteClientObject(ctx, fc.k8sclient, src.Namespace, src.Status.StarRocksFeStatus.ServiceName)
	}

	return false, nil
}

//getFeServiceName generate the name of service that access the fe.
func (fc *FeController) getFeServiceName(src *srapi.StarRocksCluster) string {
	if src.Spec.StarRocksFeSpec.Service.Name != "" {
		return src.Spec.StarRocksFeSpec.Service.Name + "-" + "service"
	}

	return src.Name + "-" + srapi.DEFAULT_FE + "-" + "service"
}

func int64ptr(n int64) *int64 {
	return &n
}