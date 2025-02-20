package cluster

import (
	"encoding/json"
	"strings"

	"github.com/rancher/lasso/pkg/dynamic"
	rancherv1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	rkev1 "github.com/rancher/rancher-operator/pkg/apis/rke.cattle.io/v1"
	mgmtcontroller "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher-operator/pkg/planner"
	"github.com/rancher/rancher-operator/pkg/util"
	"github.com/rancher/wrangler/pkg/data/convert"
	"github.com/rancher/wrangler/pkg/gvk"
	"github.com/rancher/wrangler/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	capi "sigs.k8s.io/cluster-api/api/v1alpha4"
)

func objects(cluster *rancherv1.Cluster, dynamic *dynamic.Controller, dynamicSchema mgmtcontroller.DynamicSchemaCache) (result []runtime.Object, _ error) {
	rkeCluster := rkeCluster(cluster)
	result = append(result, rkeCluster)

	capiCluster := capiCluster(cluster, rkeCluster)
	result = append(result, capiCluster)

	machineDeployments, err := machineDeployments(cluster, capiCluster, dynamic, dynamicSchema)
	if err != nil {
		return nil, err
	}

	result = append(result, machineDeployments...)
	return result, nil
}

func pruneBySchema(kind string, data map[string]interface{}, dynamicSchema mgmtcontroller.DynamicSchemaCache) error {
	ds, err := dynamicSchema.Get(strings.ToLower(kind))
	if apierror.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	for k := range data {
		if _, ok := ds.Spec.ResourceFields[k]; !ok {
			delete(data, k)
		}
	}

	return nil
}

func toMachineTemplate(nodePoolName string, cluster *rancherv1.Cluster, nodePool rancherv1.RKENodePool,
	dynamic *dynamic.Controller, dynamicSchema mgmtcontroller.DynamicSchemaCache) (runtime.Object, error) {
	apiVersion := nodePool.NodeConfig.APIVersion
	kind := nodePool.NodeConfig.Kind
	if apiVersion == "" {
		apiVersion = "rancher.cattle.io/v1"
	}

	gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
	nodeConfig, err := dynamic.Get(gvk, cluster.Namespace, nodePool.NodeConfig.Name)
	if err != nil {
		return nil, err
	}

	nodePoolData, err := util.ToMap(nodeConfig.DeepCopyObject())
	if err != nil {
		return nil, err
	}

	if err := pruneBySchema(gvk.Kind, nodePoolData, dynamicSchema); err != nil {
		return nil, err
	}

	commonData, err := convert.EncodeToMap(nodePool.RKECommonNodeConfig)
	if err != nil {
		return nil, err
	}

	nodePoolData.Set("common", commonData)

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       strings.TrimSuffix(kind, "Config") + "MachineTemplate",
			"apiVersion": "rke-node.cattle.io/v1",
			"metadata": map[string]interface{}{
				"name":      nodePoolName,
				"namespace": cluster.Namespace,
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}(nodePoolData),
				},
			},
		},
	}, nil
}

func machineDeployments(cluster *rancherv1.Cluster, capiCluster *capi.Cluster, dynamic *dynamic.Controller,
	dynamicSchema mgmtcontroller.DynamicSchemaCache) (result []runtime.Object, _ error) {
	bootstrapName := name.SafeConcatName(cluster.Name, "bootstrap", "template")

	if len(cluster.Spec.RKEConfig.NodePools) > 0 {
		result = append(result, &rkev1.RKEBootstrapTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cluster.Namespace,
				Name:      bootstrapName,
			},
		})
	}

	for _, nodePool := range cluster.Spec.RKEConfig.NodePools {
		if nodePool.Name == "" || nodePool.NodeConfig == nil || nodePool.NodeConfig.Name == "" || nodePool.NodeConfig.Kind == "" {
			continue
		}

		nodePoolName := name.SafeConcatName(cluster.Name, "nodepool", nodePool.Name)

		machineTemplate, err := toMachineTemplate(nodePoolName, cluster, nodePool, dynamic, dynamicSchema)
		if err != nil {
			return nil, err
		}

		result = append(result, machineTemplate)

		machineDeployment := &capi.MachineDeployment{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: cluster.Namespace,
				Name:      nodePoolName,
			},
			Spec: capi.MachineDeploymentSpec{
				ClusterName: capiCluster.Name,
				Replicas:    nodePool.Quantity,
				Template: capi.MachineTemplateSpec{
					ObjectMeta: capi.ObjectMeta{
						Labels:      map[string]string{},
						Annotations: map[string]string{},
					},
					Spec: capi.MachineSpec{
						ClusterName: capiCluster.Name,
						Bootstrap: capi.Bootstrap{
							ConfigRef: &corev1.ObjectReference{
								Kind:       "RKEBootstrapTemplate",
								Namespace:  cluster.Namespace,
								Name:       bootstrapName,
								APIVersion: "rke.cattle.io/v1",
							},
						},
						InfrastructureRef: corev1.ObjectReference{
							Kind:       machineTemplate.GetObjectKind().GroupVersionKind().Kind,
							Namespace:  cluster.Namespace,
							Name:       nodePoolName,
							APIVersion: "rke-node.cattle.io/v1",
						},
					},
				},
				Paused: nodePool.Paused,
			},
		}
		if nodePool.RollingUpdate != nil {
			machineDeployment.Spec.Strategy = &capi.MachineDeploymentStrategy{
				Type: capi.RollingUpdateMachineDeploymentStrategyType,
				RollingUpdate: &capi.MachineRollingUpdateDeployment{
					MaxUnavailable: nodePool.RollingUpdate.MaxUnavailable,
					MaxSurge:       nodePool.RollingUpdate.MaxSurge,
				},
			}
		}

		if defaultTrue(nodePool.EtcdRole) {
			machineDeployment.Spec.Template.Labels[planner.EtcdRoleLabel] = "true"
		}

		if defaultTrue(nodePool.ControlPlaneRole) {
			machineDeployment.Spec.Template.Labels[planner.ControlPlaneRoleLabel] = "true"
			machineDeployment.Spec.Template.Labels[capi.MachineControlPlaneLabelName] = "true"
		}

		if len(nodePool.Labels) > 0 {
			if err := assign(machineDeployment.Spec.Template.Annotations, planner.LabelsAnnotation, nodePool.Labels); err != nil {
				return nil, err
			}
		}

		if len(nodePool.Taints) > 0 {
			if err := assign(machineDeployment.Spec.Template.Annotations, planner.TaintsAnnotation, nodePool.Taints); err != nil {
				return nil, err
			}
		}

		result = append(result, machineDeployment)
	}

	return result, nil
}

func defaultTrue(b *bool) bool {
	if b == nil {
		return true
	}
	return *b
}

func assign(labels map[string]string, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	labels[key] = string(data)
	return nil
}

func rkeCluster(cluster *rancherv1.Cluster) *rkev1.RKECluster {
	return &rkev1.RKECluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
		Spec: rkev1.RKEClusterSpec{
			CloudCredentialSecretName: cluster.Spec.CloudCredentialSecretName,
			RKEClusterSpecCommon: rkev1.RKEClusterSpecCommon{
				UpgradeStrategy: rkev1.ClusterUpgradeStrategy{},
			},
			KubernetesVersion:     cluster.Spec.KubernetesVersion,
			ManagementClusterName: cluster.Status.ClusterName,
		},
	}
}

func capiCluster(cluster *rancherv1.Cluster, rkeCluster *rkev1.RKECluster) *capi.Cluster {
	gvk, err := gvk.Get(rkeCluster)
	if err != nil {
		// this is a build issue if it happens
		panic(err)
	}

	apiVersion, kind := gvk.ToAPIVersionAndKind()

	return &capi.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
		Spec: capi.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				Kind:       kind,
				Namespace:  rkeCluster.Namespace,
				Name:       rkeCluster.Name,
				APIVersion: apiVersion,
			},
		},
	}
}
