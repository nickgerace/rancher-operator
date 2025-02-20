package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/lasso/pkg/dynamic"
	rancherv1 "github.com/rancher/rancher-operator/pkg/apis/rancher.cattle.io/v1"
	v1 "github.com/rancher/rancher-operator/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher-operator/pkg/clients"
	mgmtcontroller "github.com/rancher/rancher-operator/pkg/generated/controllers/management.cattle.io/v3"
	rocontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/rancher.cattle.io/v1"
	clustercontrollers "github.com/rancher/rancher-operator/pkg/generated/controllers/rke.cattle.io/v1"
	corecontrollers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/kstatus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	byNodeInfra = "by-node-infra"
)

type handler struct {
	dynamic           *dynamic.Controller
	dynamicSchema     mgmtcontroller.DynamicSchemaCache
	clusterClient     clustercontrollers.RKEClusterClient
	clusterCache      rocontrollers.ClusterCache
	clusterController rocontrollers.ClusterController
	secretCache       corecontrollers.SecretCache
	secretClient      corecontrollers.SecretClient
}

func Register(ctx context.Context, clients *clients.Clients) {
	h := handler{
		dynamic:           clients.Dynamic,
		dynamicSchema:     clients.Management.DynamicSchema().Cache(),
		secretCache:       clients.Core.Secret().Cache(),
		secretClient:      clients.Core.Secret(),
		clusterClient:     clients.RKE.RKECluster(),
		clusterCache:      clients.Cluster.Cluster().Cache(),
		clusterController: clients.Cluster.Cluster(),
	}

	clients.RKE.RKECluster().OnChange(ctx, "rke", h.UpdateSpec)
	clients.Dynamic.OnChange(ctx, "rke", matchRKENodeGroup, h.infraWatch)
	clients.Cluster.Cluster().Cache().AddIndexer(byNodeInfra, byNodeInfraIndex)

	clustercontrollers.RegisterRKEClusterStatusHandler(ctx,
		clients.RKE.RKECluster(),
		"",
		"rke-cluster",
		h.OnChange)

	rocontrollers.RegisterClusterGeneratingHandler(ctx,
		clients.Cluster.Cluster(),
		clients.Apply.
			WithSetID("rke-cluster").
			WithSetOwnerReference(false, false).
			WithDynamicLookup().
			WithCacheTypes(
				clients.CAPI.Cluster(),
				clients.CAPI.MachineDeployment(),
				clients.RKE.RKECluster(),
				clients.RKE.RKEBootstrapTemplate(),
			),
		"",
		"rke-cluster",
		h.OnRancherClusterChange,
		nil)
}

func byNodeInfraIndex(obj *rancherv1.Cluster) ([]string, error) {
	if obj.Status.ClusterName == "" || obj.Spec.RKEConfig == nil {
		return nil, nil
	}

	var result []string
	for _, np := range obj.Spec.RKEConfig.NodePools {
		if np.NodeConfig == nil {
			continue
		}
		result = append(result, toInfraRefKey(*np.NodeConfig, obj.Namespace))
	}

	return result, nil
}

func toInfraRefKey(ref corev1.ObjectReference, namespace string) string {
	if ref.APIVersion == "" {
		ref.APIVersion = "rancher.cattle.io/v1"
	}
	return fmt.Sprintf("%s/%s/%s/%s", ref.APIVersion, ref.Kind, namespace, ref.Name)
}

func matchRKENodeGroup(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "rancher.cattle.io" &&
		strings.HasSuffix(gvk.Kind, "Config")
}

func (h *handler) infraWatch(obj runtime.Object) (runtime.Object, error) {
	if obj == nil {
		return nil, nil
	}

	typeInfo, err := meta.TypeAccessor(obj)
	if err != nil {
		return nil, err
	}

	meta, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	indexKey := toInfraRefKey(corev1.ObjectReference{
		Kind:       typeInfo.GetKind(),
		Namespace:  meta.GetNamespace(),
		Name:       meta.GetName(),
		APIVersion: typeInfo.GetAPIVersion(),
	}, meta.GetNamespace())
	clusters, err := h.clusterCache.GetByIndex(byNodeInfra, indexKey)
	if err != nil {
		return nil, err
	}

	for _, cluster := range clusters {
		h.clusterController.Enqueue(cluster.Namespace, cluster.Name)
	}

	return obj, nil
}

func (h *handler) UpdateSpec(key string, cluster *v1.RKECluster) (*v1.RKECluster, error) {
	if cluster == nil {
		return nil, nil
	}

	if cluster.Spec.ControlPlaneEndpoint == nil {
		cluster := cluster.DeepCopy()
		cluster.Spec.ControlPlaneEndpoint = &v1.Endpoint{
			Host: "localhost",
			Port: 6443,
		}
		return h.clusterClient.Update(cluster)
	}

	return cluster, nil
}

func (h *handler) OnChange(obj *v1.RKECluster, status v1.RKEClusterStatus) (v1.RKEClusterStatus, error) {
	status.Ready = true
	kstatus.SetActive(&status)
	return status, nil
}

func (h *handler) OnRancherClusterChange(obj *rancherv1.Cluster, status rancherv1.ClusterStatus) ([]runtime.Object, rancherv1.ClusterStatus, error) {
	if obj.Spec.RKEConfig == nil || obj.Status.ClusterName == "" {
		return nil, status, nil
	}
	objs, err := objects(obj, h.dynamic, h.dynamicSchema)
	return objs, status, err
}
