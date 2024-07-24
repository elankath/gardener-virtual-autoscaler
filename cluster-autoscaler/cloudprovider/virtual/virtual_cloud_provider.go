package virtual

import (
	"cmp"
	"context"
	"fmt"
	"github.com/elankath/gardener-scaling-common"
	"github.com/elankath/gardener-scaling-common/clientutil"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	kubeletapis "k8s.io/kubelet/pkg/apis"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"maps"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type VirtualNodeGroup struct {
	gsc.NodeGroupInfo
	nonNamespacedName string
	nodeTemplate      gsc.NodeTemplate
	instances         map[string]cloudprovider.Instance
	clientSet         *kubernetes.Clientset
}

var _ cloudprovider.CloudProvider = (*VirtualCloudProvider)(nil)
var _ cloudprovider.NodeGroup = (*VirtualNodeGroup)(nil)

const GPULabel = "virtual/gpu"

type VirtualCloudProvider struct {
	launchTime             time.Time
	config                 *gsc.AutoscalerConfig
	configPath             string
	configLastModifiedTime time.Time
	resourceLimiter        *cloudprovider.ResourceLimiter
	clientSet              *kubernetes.Clientset
	virtualNodeGroups      map[string]*VirtualNodeGroup
}

func BuildVirtual(opts config.AutoscalingOptions, do cloudprovider.NodeGroupDiscoveryOptions, rl *cloudprovider.ResourceLimiter) cloudprovider.CloudProvider {

	if opts.CloudProviderName != "virtual" {
		return nil
	}

	kubeConfigPath := opts.KubeClientOpts.KubeConfigPath

	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		klog.Fatalf("cannot build config from kubeConfigPath: %s, error: %s", kubeConfigPath, err.Error())
	}

	config.Burst = opts.KubeClientOpts.KubeClientBurst

	config.QPS = opts.KubeClientOpts.KubeClientQPS
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("failed to create the client: %s", err.Error())
	}

	clusterInfoPath := os.Getenv("GARDENER_CLUSTER_INFO")

	if clusterInfoPath != "" {
		cloudProvider, err := InitializeFromGardenerCluster(clusterInfoPath, clientSet, rl)
		if err != nil {
			klog.Fatalf("cannot initialize virtual autoscaler from gardener cluster info: %s", err)
			return nil
		}
		return cloudProvider
	}

	//TODO replace with configmap
	virtualAutoscalerConfigPath := os.Getenv("VIRTUAL_AUTOSCALER_CONFIG")
	if virtualAutoscalerConfigPath == "" {
		virtualAutoscalerConfigPath = "/tmp/vas-config.json"
		klog.Warningf("VIRTUAL_AUTOSCALER_CONFIG not set. Assuming %s", virtualAutoscalerConfigPath)
	}
	cloudProvider, err := InitializeFromVirtualConfig(virtualAutoscalerConfigPath, clientSet, rl)
	if err != nil {
		klog.Fatalf("cannot initialize virtual autoscaler from virtual autoscaler config path: %s", err)
		return nil
	}
	return cloudProvider

}

func AsSyncMap(mp map[string]*VirtualNodeGroup) (sMap sync.Map) {
	for k, v := range mp {
		sMap.Store(k, v)
	}
	return
}

func InitializeFromVirtualConfig(virtualAutoscalerConfigPath string, clientSet *kubernetes.Clientset, rl *cloudprovider.ResourceLimiter) (*VirtualCloudProvider, error) {
	return &VirtualCloudProvider{
		launchTime: time.Now(),
		config: &gsc.AutoscalerConfig{
			NodeTemplates: make(map[string]gsc.NodeTemplate),
			NodeGroups:    make(map[string]gsc.NodeGroupInfo),
		},
		configPath:      virtualAutoscalerConfigPath,
		resourceLimiter: rl,
		clientSet:       clientSet,
	}, nil
}

func buildVirtualNodeGroups(clientSet *kubernetes.Clientset, clusterInfo *gsc.AutoscalerConfig) (map[string]*VirtualNodeGroup, error) {
	virtualNodeGroups := make(map[string]*VirtualNodeGroup)
	for name, ng := range clusterInfo.NodeGroups {
		names := strings.Split(name, ".")
		if len(names) <= 1 {
			return nil, fmt.Errorf("cannot split nodegroup name by '.' seperator for %s", name)
		}
		virtualNodeGroup := VirtualNodeGroup{
			NodeGroupInfo:     ng,
			nonNamespacedName: names[1],
			nodeTemplate:      gsc.NodeTemplate{},
			instances:         make(map[string]cloudprovider.Instance),
			clientSet:         clientSet,
		}
		//populateNodeTemplateTaints(nodeTemplates,mcdData)
		virtualNodeGroups[name] = &virtualNodeGroup
	}
	err := populateNodeTemplates(virtualNodeGroups, clusterInfo.NodeTemplates)
	if err != nil {
		klog.Fatalf("cannot construct the virtual cloud provider: %s", err.Error())
	}
	return virtualNodeGroups, nil
}

func InitializeFromGardenerCluster(clusterInfoPath string, clientSet *kubernetes.Clientset, rl *cloudprovider.ResourceLimiter) (*VirtualCloudProvider, error) {
	clusterInfo, err := readInitClusterInfo(clusterInfoPath)
	if err != nil {
		klog.Fatalf("cannot build the virtual cloud provider: %s", err.Error())
	}
	virtualNodeGroups, err := buildVirtualNodeGroups(clientSet, &clusterInfo)
	if err != nil {
		return nil, err
	}
	return &VirtualCloudProvider{
		virtualNodeGroups: virtualNodeGroups,
		config:            &clusterInfo,
		resourceLimiter:   rl,
		clientSet:         clientSet,
	}, nil
}

func getIntOrString(val any) intstr.IntOrString {
	var valIntOrString intstr.IntOrString
	if reflect.TypeOf(val) == reflect.TypeOf("") {
		valIntOrString = intstr.FromString(val.(string))
	} else {
		if reflect.TypeOf(val) == reflect.TypeOf(0.0) {
			valIntOrString = intstr.FromInt32(int32(val.(float64)))
		} else {
			valIntOrString = intstr.FromInt32(int32(val.(int)))
		}
	}
	return valIntOrString
}

func getWorkerPoolInfo(workerPool map[string]any) (wP gsc.WorkerPoolInfo, err error) {
	architecture := workerPool["architecture"].(string)
	machineType := workerPool["machineType"].(string)
	minimum := workerPool["minimum"].(int64)
	maximum := workerPool["maximum"].(int64)
	maxSurge, err := gsc.AsIntOrString(workerPool["maxSurge"])
	if err != nil {
		return
	}
	maxUnavailable, err := gsc.AsIntOrString(workerPool["maxUnavailable"])
	if err != nil {
		return
	}
	zonesVal := workerPool["zones"].([]any)
	var zones []string
	for _, zone := range zonesVal {
		zones = append(zones, zone.(string))
	}
	wP = gsc.WorkerPoolInfo{
		MachineType:    machineType,
		Architecture:   architecture,
		Minimum:        int(minimum),
		Maximum:        int(maximum),
		MaxSurge:       maxSurge,
		MaxUnavailable: maxUnavailable,
		Zones:          zones,
	}
	return
}

func getWorkerPoolsFromShootWorker(workerDataMap map[string]any) (virtualWorkerPools []gsc.WorkerPoolInfo, err error) {
	workerPools := workerDataMap["spec"].(map[string]any)["pools"].([]any)
	for _, pool := range workerPools {
		var wp gsc.WorkerPoolInfo
		wp, err = getWorkerPoolInfo(pool.(map[string]any))
		if err != nil {
			return
		}
		virtualWorkerPools = append(virtualWorkerPools, wp)
	}
	return
}

func ResourceListFromMap(input map[string]any) (corev1.ResourceList, error) {
	resourceList := corev1.ResourceList{}

	for key, value := range input {
		// Convert the value to a string
		strValue, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("value for key %s is not a string", key)
		}

		// Parse the string value into a Quantity
		quantity, err := resource.ParseQuantity(strValue)
		if err != nil {
			return nil, fmt.Errorf("error parsing quantity for key %s: %v", key, err)
		}
		quantity, err = gsc.NormalizeQuantity(quantity)
		if err != nil {
			return nil, fmt.Errorf("cannot normalize quantity %q: %w", quantity, err)
		}
		// Assign the quantity to the ResourceList
		resourceList[corev1.ResourceName(key)] = quantity
	}

	return resourceList, nil
}

func getVirtualNodeTemplateFromMCC(mcc map[string]any) (nt gsc.NodeTemplate, err error) {
	nodeTemplate := mcc["nodeTemplate"].(map[string]any)
	//providerSpec := mcc["providerSpec"].(map[string]any)
	metadata := mcc["metadata"].(map[string]any)
	capacity, err := ResourceListFromMap(nodeTemplate["capacity"].(map[string]any))
	if err != nil {
		return
	}
	//cpuVal := nodeTemplate["capacity"].(map[string]any)["cpu"].(string)
	//gpuVal := nodeTemplate["capacity"].(map[string]any)["gpu"].(string)
	//memoryVal := nodeTemplate["capacity"].(map[string]any)["memory"].(string)
	instanceType := nodeTemplate["instanceType"].(string)
	region := nodeTemplate["region"].(string)
	zone := nodeTemplate["zone"].(string)
	//tags := providerSpec["tags"].(map[string]any)
	name := metadata["name"].(string)

	//tagsStrMap := make(map[string]string)
	//for tagKey, tagVal := range tags {
	//	tagsStrMap[tagKey] = tagVal.(string)
	//}

	//cpu, err := resource.ParseQuantity(cpuVal)
	//if err != nil {
	//	return
	//}
	//gpu, err := resource.ParseQuantity(gpuVal)
	//if err != nil {
	//	return
	//}
	//memory, err := resource.ParseQuantity(memoryVal)
	//if err != nil {
	//	return
	//}

	nt = gsc.NodeTemplate{
		Name: name,
		//CPU:          cpu,
		//GPU:          gpu,
		//Memory:       memory,
		Capacity:     capacity,
		InstanceType: instanceType,
		Region:       region,
		Zone:         zone,
		//Tags:         tagsStrMap,
	}
	return
}

func getNodeTemplatesFromMCC(mccData map[string]any) (map[string]gsc.NodeTemplate, error) {
	mccList := mccData["items"].([]any)
	var nodeTemplates []gsc.NodeTemplate
	for _, mcc := range mccList {
		nodeTemplate, err := getVirtualNodeTemplateFromMCC(mcc.(map[string]any))
		if err != nil {
			return nil, fmt.Errorf("cannot build nodeTemplate: %w", err)
		}
		nodeTemplates = append(nodeTemplates, nodeTemplate)
	}
	namespace := mccList[0].(map[string]any)["metadata"].(map[string]any)["namespace"].(string)
	return lo.KeyBy(nodeTemplates, func(item gsc.NodeTemplate) string {
		name := item.Name
		idx := strings.LastIndex(name, "-")
		// mcc name - shoot--i585976--suyash-local-worker-1-z1-0af3f , we omit the hash from the mcc name to match it with the nodegroup name
		trimmedName := name[0:idx]
		return fmt.Sprintf("%s.%s", namespace, trimmedName)
	}), nil
}

func getNodeGroupFromMCD(mcd gsc.MachineDeploymentInfo) gsc.NodeGroupInfo {
	name := mcd.Name
	namespace := mcd.Namespace
	return gsc.NodeGroupInfo{
		Name:       fmt.Sprintf("%s.%s", namespace, name),
		PoolName:   mcd.PoolName,
		Zone:       mcd.Zone,
		TargetSize: mcd.Replicas,
		MinSize:    0,
		MaxSize:    0,
	}
}

func mapToNodeGroups(mcds []gsc.MachineDeploymentInfo) map[string]gsc.NodeGroupInfo {
	var nodeGroups []gsc.NodeGroupInfo
	for _, mcd := range mcds {
		nodeGroups = append(nodeGroups, getNodeGroupFromMCD(mcd))
	}
	return lo.KeyBy(nodeGroups, func(item gsc.NodeGroupInfo) string {
		return item.Name
	})
}

func parseCASettingsInfo(caDeploymentData map[string]any) (caSettings gsc.CASettingsInfo, err error) {
	caSettings.NodeGroupsMinMax = make(map[string]gsc.MinMax)
	containersVal, err := gsc.GetInnerMapValue(caDeploymentData, "spec", "template", "spec", "containers")
	if err != nil {
		return
	}
	containers := containersVal.([]any)
	if len(containers) == 0 {
		err = fmt.Errorf("len of containers is zero, no CA container found")
		return
	}
	caContainer := containers[0].(map[string]any)
	caCommands := caContainer["command"].([]any)
	for _, commandVal := range caCommands {
		command := commandVal.(string)
		vals := strings.Split(command, "=")
		if len(vals) <= 1 {
			continue
		}
		key := vals[0]
		val := vals[1]
		switch key {
		case "--max-graceful-termination-sec":
			caSettings.MaxGracefulTerminationSeconds, err = strconv.Atoi(val)
		case "--max-node-provision-time":
			caSettings.MaxNodeProvisionTime, err = time.ParseDuration(val)
		case "--scan-interval":
			caSettings.ScanInterval, err = time.ParseDuration(val)
		case "--max-empty-bulk-delete":
			caSettings.MaxEmptyBulkDelete, err = strconv.Atoi(val)
		case "--new-pod-scale-up-delay":
			caSettings.NewPodScaleUpDelay, err = time.ParseDuration(val)
		case "--nodes":
			var ngMinMax gsc.MinMax
			ngVals := strings.Split(val, ":")
			ngMinMax.Min, err = strconv.Atoi(ngVals[0])
			ngMinMax.Max, err = strconv.Atoi(ngVals[1])
			caSettings.NodeGroupsMinMax[ngVals[2]] = ngMinMax
		}
		if err != nil {
			return
		}
	}
	return
}

func readInitClusterInfo(clusterInfoPath string) (cI gsc.AutoscalerConfig, err error) {
	workerJsonFile := fmt.Sprintf("%s/shoot-worker.json", clusterInfoPath)
	data, err := os.ReadFile(workerJsonFile)
	if err != nil {
		klog.Errorf("cannot read the shoot json file: %s", err.Error())
		return
	}

	var workerDataMap map[string]any
	err = json.Unmarshal(data, &workerDataMap)
	if err != nil {
		klog.Errorf("cannot unmarshal the worker json: %s", err.Error())
		return
	}
	if err != nil {
		klog.Errorf("cannot parse the worker pools: %s", err.Error())
		return
	}

	machineClassesJsonPath := fmt.Sprintf("%s/machine-classes.json", clusterInfoPath)
	data, err = os.ReadFile(machineClassesJsonPath)
	if err != nil {
		klog.Errorf("cannot read the mcc json file: %s", err.Error())
		return
	}
	var mccData map[string]any
	err = json.Unmarshal(data, &mccData)
	if err != nil {
		klog.Errorf("cannot unmarshal the mcc json: %s", err.Error())
		return
	}
	cI.NodeTemplates, err = getNodeTemplatesFromMCC(mccData)
	if err != nil {
		klog.Errorf("cannot build the nodeTemplates: %s", err.Error())
	}

	//mcdJsonFile := fmt.Sprintf("%s/mcds.json", clusterInfoPath)
	//_, err = os.ReadFile(mcdJsonFile)
	//if err != nil {
	//	klog.Errorf("cannot read the mcd json file: %s", err.Error())
	//	return
	//}
	machineDeploymentsJsonPath := fmt.Sprintf("%s/machine-deployments.json", clusterInfoPath)
	machineDeployments, err := readMachineDeploymentInfos(machineDeploymentsJsonPath)
	if err != nil {
		klog.Errorf("readMachineDeploymentInfos error: %s", err.Error())
		return
	}

	populateNodeTemplatesFromMCD(machineDeployments, cI.NodeTemplates)
	cI.NodeGroups = mapToNodeGroups(machineDeployments)

	caDeploymentJsonFile := fmt.Sprintf("%s/ca-deployment.json", clusterInfoPath)
	data, err = os.ReadFile(caDeploymentJsonFile)
	if err != nil {
		klog.Errorf("cannot read the ca-deployment json file: %s", err.Error())
		return
	}
	var caDeploymentData map[string]any
	err = json.Unmarshal(data, &caDeploymentData)
	cI.CASettings, err = parseCASettingsInfo(caDeploymentData)
	if err != nil {
		klog.Errorf("cannot parse the ca settings from deployment json: %s", err.Error())
		return
	}
	err = cI.Init()
	return
}

func populateNodeTemplatesFromMCD(mcds []gsc.MachineDeploymentInfo, nodeTemplates map[string]gsc.NodeTemplate) {
	for _, mcd := range mcds {
		templateName := fmt.Sprintf("%s.%s", mcd.Namespace, mcd.Name)
		nodeTemplate := nodeTemplates[templateName]
		nodeTemplate.Labels = mcd.Labels
		nodeTemplate.Taints = mcd.Taints
		nodeTemplates[templateName] = nodeTemplate
	}
}

func populateNodeTemplates(nodeGroups map[string]*VirtualNodeGroup, nodeTemplates map[string]gsc.NodeTemplate) error {
	for name, template := range nodeTemplates {
		ng, ok := nodeGroups[name]
		if !ok {
			return fmt.Errorf("nodegroup name not found: %s", name)
		}
		ng.nodeTemplate = template
		nodeGroups[name] = ng
	}
	return nil
}

func (vcp *VirtualCloudProvider) Name() string {
	return cloudprovider.VirtualProviderName
}

func (vcp *VirtualCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	nodeGroups := make([]cloudprovider.NodeGroup, len(vcp.virtualNodeGroups))
	count := 0
	for _, v := range vcp.virtualNodeGroups {
		nodeGroups[count] = v
		count++
	}
	return nodeGroups
}

type poolKey struct {
	poolName string
	zone     string
}

func (vcp *VirtualCloudProvider) getNodeGroupsByPoolKey() map[poolKey]*VirtualNodeGroup {
	poolKeyMap := make(map[poolKey]*VirtualNodeGroup)
	for _, vng := range vcp.virtualNodeGroups {
		pk := poolKey{
			poolName: vng.PoolName,
			zone:     vng.Zone,
		}
		poolKeyMap[pk] = vng
	}
	return poolKeyMap
}

func getZonefromNodeLabels(nodeLabel map[string]string) (string, error) {
	//TODO do include zone labels from other providers
	zone, ok := nodeLabel["topology.kubernetes.io/zone"]
	if ok {
		return zone, nil
	}
	zone, ok = nodeLabel["topology.gke.io/zone"]
	if ok {
		return zone, nil
	}
	return "", fmt.Errorf("no eligible zone label available for node %q", nodeLabel["kubernetes.io/hostname"])
}
func (vcp *VirtualCloudProvider) NodeGroupForNode(node *corev1.Node) (cloudprovider.NodeGroup, error) {
	if len(vcp.config.NodeGroups) == 0 {
		klog.Warning("virtual autoscaler has not been initialized with nodes")
		return nil, nil
	}
	ctx := context.Background()
	poolKeyMap := vcp.getNodeGroupsByPoolKey()
	nodeInCluster, err := vcp.clientSet.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("can't find VirtualNodeGroup for node with name %q: %w", node.Name, err)
	}
	nodeLabels := make(map[string]string)
	if node.Labels != nil {
		maps.Copy(nodeLabels, nodeLabels)
	}
	if nodeInCluster.Labels != nil {
		maps.Copy(nodeLabels, nodeInCluster.Labels)
	}
	zone, err := getZonefromNodeLabels(nodeLabels)
	if err != nil {
		return nil, fmt.Errorf("cant find VirtualNodeGroup for node with with name %q: %w", node.Name, err)
	}
	nodePoolKey := poolKey{
		poolName: nodeLabels["worker.gardener.cloud/pool"],
		zone:     zone,
	}
	nodeGroup, ok := poolKeyMap[nodePoolKey]
	if !ok {
		return nil, fmt.Errorf("cant find VirtualNodeGroup for node with with name %q", node.Name)
	}
	return nodeGroup, nil
}

func (vcp *VirtualCloudProvider) HasInstance(node *corev1.Node) (bool, error) {
	return true, cloudprovider.ErrNotImplemented
}

func (vcp *VirtualCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

func (vcp *VirtualCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return []string{}, nil
}

func (vcp *VirtualCloudProvider) NewNodeGroup(machineType string, labels map[string]string, systemLabels map[string]string, taints []corev1.Taint, extraResources map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

func (vcp *VirtualCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return vcp.resourceLimiter, nil
}

func (vcp *VirtualCloudProvider) GPULabel() string {
	return GPULabel
}

func (vcp *VirtualCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return nil
}

func (vcp *VirtualCloudProvider) GetNodeGpuConfig(node *corev1.Node) *cloudprovider.GpuConfig {
	return nil
}

func (vcp *VirtualCloudProvider) Cleanup() error {
	return nil
}

func checkAndGetFileLastModifiedTime(filePath string) (exist bool, lastModifiedTime time.Time, err error) {
	file, err := os.Stat(filePath)
	if err != nil {
		return
	}
	exist = true
	lastModifiedTime = file.ModTime()
	return
}

func loadAutoscalerConfig(filePath string) (config gsc.AutoscalerConfig, err error) {
	bytes, err := os.ReadFile(filePath)
	if err != nil {
		return
	}

	err = json.Unmarshal(bytes, &config)
	if err != nil {
		return
	}

	return
}

func (vcp *VirtualCloudProvider) checkAndReloadConfig() (bool, error) {
	exist, lastModifiedTime, err := checkAndGetFileLastModifiedTime(vcp.configPath)
	if err != nil {
		return false, fmt.Errorf("error looking up the virtual autoscaler autoscalerConfig at path: %s, error: %s", vcp.configPath, err)
	}
	if !exist {
		klog.Warningf("virtual autoscaler autoscalerConfig is still missing at path: %s", vcp.configPath)
		return false, nil
	}
	if vcp.launchTime.After(lastModifiedTime) {
		klog.Warningf("Ignoring Old virtual autoscalerConfig at path %q with time %q created before the CA launch time %q", vcp.configPath, lastModifiedTime, vcp.launchTime)
		return false, nil
	}
	autoscalerConfig, err := loadAutoscalerConfig(vcp.configPath)
	if err != nil {
		klog.Errorf("failed to construct the virtual autoscaler autoscalerConfig from file: %s, error: %s", vcp.configPath, err)
		return false, err
	}
	if vcp.config.Hash != autoscalerConfig.Hash {
		vcp.config = &autoscalerConfig
		vcp.configLastModifiedTime = lastModifiedTime
		return true, nil
	}
	return false, nil
}

func (vcp *VirtualCloudProvider) reloadVirtualNodeGroups() error {
	virtualNodeGroups, err := buildVirtualNodeGroups(vcp.clientSet, vcp.config)
	if err != nil {
		return err
	}
	vcp.virtualNodeGroups = virtualNodeGroups
	return nil
}

func adjustNode(clientSet *kubernetes.Clientset, nodeName string, nodeStatus corev1.NodeStatus) error {

	nd, err := clientSet.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cannot get node with name %q: %w", nd.Name, err)
	}
	nd.Spec.Taints = lo.Filter(nd.Spec.Taints, func(item corev1.Taint, index int) bool {
		return item.Key != "node.kubernetes.io/not-ready"
	})
	nd, err = clientSet.CoreV1().Nodes().Update(context.Background(), nd, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("cannot update node with name %q: %w", nd.Name, err)
	}
	//nd.Status.Conditions = cloudprovider.BuildReadyConditions()
	//nd.Status.Phase = corev1.NodeRunning
	//TODO set the nodeInfo in node status
	nd.Status = nodeStatus
	nd.Status.Phase = corev1.NodeRunning
	nd, err = clientSet.CoreV1().Nodes().UpdateStatus(context.Background(), nd, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("cannot update the status of node with name %q: %w", nd.Name, err)
	}
	return nil
}

func (vcp *VirtualCloudProvider) refreshNodes() error {
	configNodeInfos := vcp.config.ExistingNodes
	ctx := context.Background()
	err := vcp.synchronizeNodes(ctx, configNodeInfos)
	if err != nil {
		return err
	}
	return nil
}

func (vcp *VirtualCloudProvider) synchronizeNodes(ctx context.Context, configNodeInfos []gsc.NodeInfo) error {
	snapshotNodeInfosByName := lo.Associate(configNodeInfos, func(item gsc.NodeInfo) (string, struct{}) {
		return item.Name, struct{}{}
	})
	virtualNodes, err := clientutil.ListAllNodes(ctx, vcp.clientSet)
	if err != nil {
		return fmt.Errorf("cannot list the nodes in virtual cluster: %w", err)
	}
	virtualNodesMap := lo.KeyBy(virtualNodes, func(item corev1.Node) string {
		return item.Name
	})

	for _, vn := range virtualNodes {
		_, ok := snapshotNodeInfosByName[vn.Name]
		if ok {
			continue
		}
		err := vcp.clientSet.CoreV1().Nodes().Delete(ctx, vn.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("cannot delete the virtual node %q: %w", vn.Name, err)
		}
		//delete(virtualNodesMap, vn.Name)
		klog.V(3).Infof("synchronizeNodes deleted the virtual node %q", vn.Name)
	}

	for _, nodeInfo := range configNodeInfos {
		oldVNode, exists := virtualNodesMap[nodeInfo.Name]
		var sameLabels, sameTaints bool
		if exists {
			sameLabels = maps.Equal(oldVNode.Labels, nodeInfo.Labels)
			sameTaints = slices.EqualFunc(oldVNode.Spec.Taints, nodeInfo.Taints, gsc.IsEqualTaint)
		}
		if exists && sameLabels && sameTaints {
			continue
		}
		node := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeInfo.Name,
				Namespace: nodeInfo.Namespace,
				Labels:    nodeInfo.Labels,
			},
			Spec: corev1.NodeSpec{
				Taints:     nodeInfo.Taints,
				ProviderID: nodeInfo.ProviderID,
			},
			Status: corev1.NodeStatus{
				Capacity:    nodeInfo.Capacity,
				Allocatable: nodeInfo.Allocatable,
			},
		}
		nodeStatus := node.Status
		if !exists {
			_, err = vcp.clientSet.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				klog.Warningf("synchronizeNodes: node already exists. updating node %q", node.Name)
				_, err = vcp.clientSet.CoreV1().Nodes().Update(context.Background(), &node, metav1.UpdateOptions{})
			}
			if err == nil {
				klog.V(3).Infof("synchronizeNodes created node %q", node.Name)
			}
		} else {
			_, err = vcp.clientSet.CoreV1().Nodes().Update(context.Background(), &node, metav1.UpdateOptions{})
			klog.V(3).Infof("synchronizeNodes updated node %q", node.Name)
		}
		if err != nil {
			return fmt.Errorf("synchronizeNodes cannot create/update node with name %q: %w", node.Name, err)
		}
		node.Status = nodeStatus
		node.Status.Conditions = cloudprovider.BuildReadyConditions()
		err = adjustNode(vcp.clientSet, node.Name, node.Status)
		if err != nil {
			return fmt.Errorf("synchronizeNodes cannot adjust the node with name %q: %w", node.Name, err)
		}
	}

	// Update VirtualNodeGroup.Instances
	virtualNodes, err = clientutil.ListAllNodes(ctx, vcp.clientSet)
	if err != nil {
		return fmt.Errorf("cannot list the nodes in virtual cluster: %w", err)
	}

	for _, node := range virtualNodes {
		ng, err := vcp.NodeGroupForNode(&node)
		if err != nil {
			return fmt.Errorf("synchronizeNodes can't find NodeGroup for node %q: %w", node.Name, err)
		}
		vng, ok := vcp.virtualNodeGroups[ng.Id()]
		if !ok {
			return fmt.Errorf("synchronizeNodes can't find VirtualNodeGroup with name: %w", ng.Id())
		}
		vng.instances[node.Name] = cloudprovider.Instance{
			Id: node.Name,
			Status: &cloudprovider.InstanceStatus{
				State:     cloudprovider.InstanceCreating,
				ErrorInfo: nil,
			},
		}
		klog.V(3).Infof("synchronizeNodes added instance to VirtualNodeGroup %q", vng.Name)
		time.AfterFunc(1*time.Second, func() { vng.changeCreatingInstancesToRunning(ctx) })
	}
	return nil
}

func (vcp *VirtualCloudProvider) Refresh() error {
	configReloaded, err := vcp.checkAndReloadConfig()
	if err != nil {
		return err
	}
	if len(vcp.config.NodeGroups) == 0 {
		return fmt.Errorf("virtual autoscaler is not initialized")
	}
	if configReloaded {
		err = vcp.reloadVirtualNodeGroups()
		if err != nil {
			return err
		}
	}
	if len(vcp.config.NodeGroups) == 0 {
		return nil
	}
	if configReloaded {
		err = vcp.refreshNodes()
	}
	if err != nil {
		return err
	}
	if configReloaded {
		klog.V(2).Infof("completed config reload of virtual cloud provider from path: %s", vcp.configPath)
	} else {
		klog.V(2).Infof("unchanged config for virtual cloud provider at path: %s", vcp.configPath)
	}
	return nil
}

var _ cloudprovider.CloudProvider = (*VirtualCloudProvider)(nil)

func (v *VirtualNodeGroup) MaxSize() int {
	return v.NodeGroupInfo.MaxSize
}

func (v *VirtualNodeGroup) MinSize() int {
	return v.NodeGroupInfo.MinSize
}

func (v *VirtualNodeGroup) TargetSize() (int, error) {
	klog.V(3).Infof("TargetSize() of %q is currently %d", v.Name, len(v.instances))
	return len(v.instances), nil
}

func (v *VirtualNodeGroup) changeCreatingInstancesToRunning(ctx context.Context) {
	for nn, _ := range v.instances {
		if v.instances[nn].Status.State == cloudprovider.InstanceRunning {
			continue
		}
		klog.Infof("changing the instace %s state from creating to running", v.instances[nn].Id)
		v.instances[nn].Status.State = cloudprovider.InstanceRunning
		node, err := v.clientSet.CoreV1().Nodes().Get(ctx, nn, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("changeCreatingInstancesToRunning cannot get the node object for the corresponding instance: %s", nn)
			return
		}
		node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(taint corev1.Taint) bool {
			return taint.Key == "node.kubernetes.io/not-ready"
		})
		updatedNode, err := v.clientSet.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("changeCreatingInstancesToRunning cannot update the node for corresponding instance: %s", nn)
			return
		}
		klog.Infof("changeCreatingInstancesToRunning removed the not ready taint from node: %s", updatedNode.Name)
	}
}

func (v *VirtualNodeGroup) IncreaseSize(delta int) error {
	ctx := context.Background()
	//TODO add flags for simulating provider errors ex : ResourceExhaustion
	if len(v.instances) >= v.MaxSize() {
		klog.Warningf("IncreseSize SPURIOUSLY called by CA core for %q with delta %d", v.Name, delta)
		return nil
	}
	// double check against Nodes of virtual cluster.
	klog.V(3).Infof("IncreseSize called for %q with delta %d", v.Name, delta)
	for i := 0; i < delta; i++ {
		node, err := v.buildCoreNodeFromTemplate()
		if err != nil {
			return err
		}
		v.instances[node.Name] = cloudprovider.Instance{
			Id: node.Name,
			Status: &cloudprovider.InstanceStatus{
				State:     cloudprovider.InstanceCreating,
				ErrorInfo: nil,
			},
		}
		nodeStatus := node.Status
		createdNode, err := v.clientSet.CoreV1().Nodes().Create(ctx, &node, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		klog.V(3).Infof("IncreseSize created node with name %q", node.Name)
		err = adjustNode(v.clientSet, node.Name, nodeStatus)
		if err != nil {
			return err
		}
		klog.Infof("IncreseSize created a new node with name: %s", createdNode.Name)
	}
	time.AfterFunc(3*time.Second, func() { v.changeCreatingInstancesToRunning(ctx) })
	return nil
}

func (v *VirtualNodeGroup) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

func (v *VirtualNodeGroup) DeleteNodes(nodes []*corev1.Node) error {
	ctx := context.Background()
	for _, node := range nodes {
		klog.V(3).Infof("DeleteNodes is deleting node with name %q", node.Name)
		err := v.clientSet.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		delete(v.instances, node.Name)
	}
	return nil
}

func (v *VirtualNodeGroup) DecreaseTargetSize(delta int) error {
	klog.V(3).Infof("DecreaseTargetSize called for ng %q and delta %d", v.Name, delta)
	ctx := context.Background()
	nodes, err := v.clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if delta > len(nodes.Items) {
		return fmt.Errorf("nodes to be deleted are greater than current number of nodes")
	}
	pods, err := v.clientSet.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	podsToNodesMap := lo.GroupBy(pods.Items, func(pod corev1.Pod) string {
		return pod.Spec.NodeName
	})
	var deleteNodes []*corev1.Node
	slices.SortFunc(nodes.Items, func(a, b corev1.Node) int {
		return cmp.Compare(len(podsToNodesMap[a.Name]), len(podsToNodesMap[b.Name]))
	})
	for i := 0; i < delta; i++ {
		deleteNodes = append(deleteNodes, &nodes.Items[i])
	}
	return v.DeleteNodes(deleteNodes)
}

func (v *VirtualNodeGroup) Id() string {
	return v.Name
}

func (v *VirtualNodeGroup) Debug() string {
	return fmt.Sprintf("%s (%d:%d)", v.Id(), v.MinSize(), v.MaxSize())
}

func (v *VirtualNodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	return lo.Values(v.instances), nil
}

func buildGenericLabels(template *gsc.NodeTemplate, nodeName string) map[string]string {
	result := make(map[string]string)
	// TODO: extract from MCM
	result[kubeletapis.LabelArch] = cloudprovider.DefaultArch
	result[corev1.LabelArchStable] = cloudprovider.DefaultArch

	result[kubeletapis.LabelOS] = cloudprovider.DefaultOS
	result[corev1.LabelOSStable] = cloudprovider.DefaultOS

	result[corev1.LabelInstanceType] = template.InstanceType
	result[corev1.LabelInstanceTypeStable] = template.InstanceType

	result[corev1.LabelZoneRegion] = template.Region
	result[corev1.LabelZoneRegionStable] = template.Region

	result[corev1.LabelZoneFailureDomain] = template.Zone
	result[corev1.LabelZoneFailureDomainStable] = template.Zone

	//TODO fix node name label to satisfy validation
	result[corev1.LabelHostname] = nodeName
	return result
}

func (v *VirtualNodeGroup) buildCoreNodeFromTemplate() (corev1.Node, error) {
	node := corev1.Node{}
	nodeName := fmt.Sprintf("%s-%d", v.nonNamespacedName, rand.Int63())

	node.ObjectMeta = metav1.ObjectMeta{
		Name:     nodeName,
		SelfLink: fmt.Sprintf("/api/v1/nodes/%s", nodeName),
		Labels:   map[string]string{},
	}

	node.Status = corev1.NodeStatus{
		Capacity: maps.Clone(v.nodeTemplate.Capacity),
	}
	node.Status.Capacity[corev1.ResourcePods] = resource.MustParse("110") //Fixme must take it dynamically from node object
	//node.Status.Capacity[corev1.ResourceCPU] = v.nodeTemplate.CPU
	//if v.nodeTemplate.GPU.Cmp(resource.MustParse("0")) != 0 {
	node.Status.Capacity[gpu.ResourceNvidiaGPU] = v.nodeTemplate.Capacity["gpu"]
	delete(node.Status.Capacity, "gpu")
	//}
	//node.Status.Capacity[corev1.ResourceMemory] = v.nodeTemplate.Memory
	//node.Status.Capacity[corev1.ResourceEphemeralStorage] = v.nodeTemplate.EphemeralStorage
	// added most common hugepages sizes. This will help to consider the template node while finding similar node groups
	node.Status.Capacity["hugepages-1Gi"] = *resource.NewQuantity(0, resource.DecimalSI)
	node.Status.Capacity["hugepages-2Mi"] = *resource.NewQuantity(0, resource.DecimalSI)

	node.Status.Allocatable = node.Status.Capacity

	// NodeLabels
	//TODO FIXME fix tags preventing node creation
	//node.Labels = v.nodeTemplate.Tags
	//// GenericLabels
	node.Labels = cloudprovider.JoinStringMaps(node.Labels, buildGenericLabels(&v.nodeTemplate, nodeName))
	maps.Copy(node.Labels, v.nodeTemplate.Labels)
	//node.Labels[NodeGroupLabel] = v.nonNamespacedName

	//TODO populate taints from mcd
	node.Spec.Taints = v.nodeTemplate.Taints

	node.Status.Conditions = cloudprovider.BuildReadyConditions()
	return node, nil
}

func (v *VirtualNodeGroup) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {
	coreNode, err := v.buildCoreNodeFromTemplate()
	if err != nil {
		return nil, err
	}
	//TODO Discuss it
	nodeInfo := schedulerframework.NewNodeInfo(cloudprovider.BuildKubeProxy(v.Name))
	nodeInfo.SetNode(&coreNode)
	return nodeInfo, nil
}

func (v *VirtualNodeGroup) Exist() bool {
	return true
}

func (v *VirtualNodeGroup) Create() (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrAlreadyExist
}

func (v *VirtualNodeGroup) Delete() error {
	return cloudprovider.ErrNotImplemented
}

func (v *VirtualNodeGroup) Autoprovisioned() bool {
	return false
}

func (v *VirtualNodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	//TODO copy from mcm get options
	return &defaults, nil
}

func readMachineDeploymentInfos(mcdsJsonFile string) ([]gsc.MachineDeploymentInfo, error) {
	bytes, err := os.ReadFile(mcdsJsonFile)
	if err != nil {
		return nil, err
	}
	var mcdData unstructured.Unstructured
	err = json.Unmarshal(bytes, &mcdData)
	if err != nil {
		klog.Errorf("cannot unmarshal the mcd json: %s", err.Error())
		return nil, err
	}
	items := mcdData.UnstructuredContent()["items"].([]any)
	mcdInfos := make([]gsc.MachineDeploymentInfo, len(items))
	for i, item := range items {
		itemMap := item.(map[string]any)
		itemObj := unstructured.Unstructured{Object: itemMap}
		//medataDataMap, found, err := itemObjunstructured.NestedMap(itemMap, "metadata")
		name := itemObj.GetName()
		namespace := itemObj.GetNamespace()
		specMap, found, err := unstructured.NestedMap(itemObj.UnstructuredContent(), "spec")
		if !found {
			return nil, fmt.Errorf("cannot find 'spec' inside machine deployment json with idx %d", i)
		}
		if err != nil {
			return nil, fmt.Errorf("error loading spec map inside machine deployment json with idx %d: %w", i, err)
		}
		var replicas int
		replicasVal, ok := specMap["replicas"]
		if ok {
			replicas = int(replicasVal.(int64))
		}
		nodeTemplate, found, err := unstructured.NestedMap(specMap, "template", "spec", "nodeTemplate")
		if !found {
			return nil, fmt.Errorf("cannot find nested nodeTemplate map inside machine deployment json with idx %d", i)
		}
		if err != nil {
			return nil, fmt.Errorf("error loading nested nodeTemplate map inside machine deployment json with idx %d: %w", i, err)
		}
		labels, found, err := unstructured.NestedStringMap(nodeTemplate, "metadata", "labels")
		if !found {
			return nil, fmt.Errorf("cannot find nested labels inside nodeTemplate belonging to machine deployment json with idx %d", i)
		}
		if err != nil {
			return nil, fmt.Errorf("error loading nested labels inside nodeTemplate belonging to machine deployment json with idx %d: %w", i, err)
		}
		labelsVal, found, err := unstructured.NestedMap(nodeTemplate, "metadata", "labels")
		if !found {
			return nil, fmt.Errorf("cannot find nested labels inside nodeTemplate belonging to machine deployment json with idx %d", i)
		}
		if err != nil {
			return nil, fmt.Errorf("error loading nested labels inside nodeTemplate belonging to machine deployment json with idx %d: %w", i, err)
		}
		taintsVal, found, err := unstructured.NestedSlice(nodeTemplate, "spec", "taints")
		if err != nil {
			return nil, fmt.Errorf("error loading nested taints inside nodeTemplate.spec.taints belonging to machine deployment json with idx %d: %w", i, err)
		}

		var taints []corev1.Taint
		if found {
			for _, tv := range taintsVal {
				tvMap := tv.(map[string]any)
				taints = append(taints, corev1.Taint{
					Key:       tvMap["key"].(string),
					Value:     tvMap["value"].(string),
					Effect:    corev1.TaintEffect(tvMap["effect"].(string)),
					TimeAdded: nil,
				})
			}
		}
		klog.Infof("found taints inside  nodeTemplate belonging to machine deployment json with idx %d: %s", i, taintsVal)

		class, found, err := unstructured.NestedStringMap(specMap, "template", "spec", "class")
		if !found {
			return nil, fmt.Errorf("cannot find nested class inside nodeTemplate belonging to machine deployment json with idx %d", i)
		}
		mcdInfo := gsc.MachineDeploymentInfo{
			SnapshotMeta: gsc.SnapshotMeta{
				CreationTimestamp: itemObj.GetCreationTimestamp().Time,
				SnapshotTimestamp: time.Now(),
				Name:              name,
				Namespace:         namespace,
			},
			Replicas:         replicas,
			PoolName:         labels["worker.gardener.cloud/pool"],
			Zone:             gsc.GetZone(labelsVal),
			MaxSurge:         intstr.IntOrString{},
			MaxUnavailable:   intstr.IntOrString{},
			MachineClassName: class["name"],
			Labels:           labels,
			Taints:           taints,
			Hash:             "",
		}
		mcdInfos[i] = mcdInfo
	}
	return mcdInfos, nil
}
