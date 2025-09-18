package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

const (
	maxAttempts        = 10
	stabilityThreshold = 5 * time.Minute
)

// AllocatedDevice represents a network device that has been allocated to a pod.
type AllocatedDevice struct {
	Name       string
	Attributes map[string]string
	PoolName   string
	Request    string
}

// SharedState is the data that is shared between the DRA and NRI hooks.
// It is managed by the Plugin framework and passed to the driver's hooks.
type SharedState struct {
	// PodDeviceConfig maps a pod's UID to the devices that have been allocated to it.
	PodDeviceConfig map[types.UID][]AllocatedDevice
	// PreparedData maps a pod's UID to the data that was returned by the PrepareDevice hook.
	PreparedData map[types.UID]interface{}
}

// Plugin manages the lifecycle of the DRA and NRI plugins and calls the
// hooks of the provided Driver implementation.
type Plugin struct {
	driverName string
	nodeName   string
	kubeClient kubernetes.Interface
	draPlugin  *kubeletplugin.Helper
	nriPlugin  stub.Stub

	mu          sync.Mutex
	sharedState *SharedState
}

// NewPlugin creates a new Plugin instance.
func NewPlugin(driverName, nodeName string, kubeClient kubernetes.Interface) *Plugin {
	return &Plugin{
		driverName: driverName,
		nodeName:   nodeName,
		kubeClient: kubeClient,
		sharedState: &SharedState{
			PodDeviceConfig: make(map[types.UID][]AllocatedDevice),
			PreparedData:    make(map[types.UID]interface{}),
		},
	}
}

// Start initializes and runs the DRA and NRI plugins.
func (p *Plugin) Start(ctx context.Context) error {
	driverPluginPath := filepath.Join(kubeletplugin.KubeletPluginsDir, p.driverName)
	if err := os.MkdirAll(driverPluginPath, 0750); err != nil {
		return fmt.Errorf("failed to create plugin path %s: %w", driverPluginPath, err)
	}

	kubeletOptions := []kubeletplugin.Option{
		kubeletplugin.DriverName(p.driverName),
		kubeletplugin.NodeName(p.nodeName),
		kubeletplugin.KubeClient(p.kubeClient),
	}
	draHelper, err := kubeletplugin.Start(ctx, p, kubeletOptions...)
	if err != nil {
		return fmt.Errorf("start kubelet plugin: %w", err)
	}
	p.draPlugin = draHelper

	if err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(context.Context) (bool, error) {
		status := p.draPlugin.RegistrationStatus()
		return status != nil && status.PluginRegistered, nil
	}); err != nil {
		return err
	}

	nriOptions := []stub.Option{
		stub.WithPluginName(p.driverName),
		stub.WithPluginIdx("10"),
		stub.WithOnClose(func() { klog.Infof("%s NRI plugin closed", p.driverName) }),
	}
	nriStub, err := stub.New(p, nriOptions...)
	if err != nil {
		return fmt.Errorf("failed to create NRI plugin stub: %w", err)
	}
	p.nriPlugin = nriStub

	go p.runNRIPlugin(ctx)
	go p.publishResources(ctx)

	return nil
}

// Stop gracefully stops the DRA and NRI plugins.
func (p *Plugin) Stop() {
	klog.Info("Stopping network driver plugin...")
	if p.nriPlugin != nil {
		p.nriPlugin.Stop()
	}
	if p.draPlugin != nil {
		p.draPlugin.Stop()
	}
	klog.Info("Network driver plugin stopped.")
}

// DRA plugin implementation
func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(2).Infof("PrepareResourceClaims called for %d claims", len(claims))
	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		preparedData, err := p.PrepareDevice(ctx, claim)
		if err != nil {
			results[claim.UID] = kubeletplugin.PrepareResult{Err: err}
			continue
		}
		p.mu.Lock()
		p.sharedState.PreparedData[claim.UID] = preparedData
		p.mu.Unlock()
		results[claim.UID] = kubeletplugin.PrepareResult{}
	}
	return results, nil
}

func (p *Plugin) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	klog.V(2).Infof("UnprepareResourceClaims called for %d claims", len(claims))
	errors := make(map[types.UID]error)
	for _, claim := range claims {
		if err := p.UnprepareDevice(ctx, claim); err != nil {
			errors[claim.UID] = err
		}
		p.mu.Lock()
		delete(p.sharedState.PreparedData, claim.UID)
		p.mu.Unlock()
	}
	return errors, nil
}

// HandleError is called for errors encountered in the background.
func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleError(fmt.Errorf("%s: %w", msg, err))
}

// NRI handler implementation
func (p *Plugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	klog.V(2).Info("Synchronize called")
	return nil, nil
}

func (p *Plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RunPodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)
	if networkNamespace == "" {
		return fmt.Errorf("pod %s/%s has no network namespace", pod.Namespace, pod.Name)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	devices := p.sharedState.PodDeviceConfig[podUID]
	preparedData := p.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := p.ConfigureDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) StopPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("StopPodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	networkNamespace := getNetworkNamespace(pod)

	p.mu.Lock()
	defer p.mu.Unlock()

	devices := p.sharedState.PodDeviceConfig[podUID]
	preparedData := p.sharedState.PreparedData[podUID]

	for _, device := range devices {
		if err := p.CleanupDeviceForPod(device, networkNamespace, pod, preparedData); err != nil {
			klog.Errorf("failed to cleanup device %s for pod %s: %v", device.Name, pod.Name, err)
		}
	}
	return nil
}

func (p *Plugin) RemovePodSandbox(ctx context.Context, pod *api.PodSandbox) error {
	klog.V(2).Infof("RemovePodSandbox called for pod %s/%s", pod.Namespace, pod.Name)
	podUID := types.UID(pod.Uid)
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sharedState.PodDeviceConfig, podUID)
	delete(p.sharedState.PreparedData, podUID)
	return nil
}

// Helper functions
func (p *Plugin) runNRIPlugin(ctx context.Context) {
	attempt := 0
	for attempt < maxAttempts {
		startTime := time.Now()
		if err := p.nriPlugin.Run(ctx); err != nil {
			klog.Errorf("NRI plugin failed: %v", err)
		}

		// if the plugin was stable for a while, reset the backoff counter
		if time.Since(startTime) > stabilityThreshold {
			klog.Infof("nri plugin was stable for more than %v, resetting retry counter", stabilityThreshold)
			attempt = 0
		} else {
			attempt++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			klog.Infof("Restarting NRI plugin (attempt %d/%d)", attempt, maxAttempts)
		}
	}
	klog.Fatalf("NRI plugin failed to restart after %d attempts", maxAttempts)
}

func (p *Plugin) publishResources(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			devices, err := p.GetDevices()
			if err != nil {
				klog.Errorf("failed to get devices: %v", err)
				continue
			}
			resources := resourceslice.DriverResources{
				Pools: map[string]resourceslice.Pool{
					p.nodeName: {Slices: []resourceslice.Slice{{Devices: devices}}},
				},
			}
			if err := p.draPlugin.PublishResources(ctx, resources); err != nil {
				klog.Errorf("failed to publish resources: %v", err)
			}
		}
	}
}

func getNetworkNamespace(pod *api.PodSandbox) string {
	for _, ns := range pod.Linux.GetNamespaces() {
		if ns.Type == "network" {
			return ns.Path
		}
	}
	return ""
}

// Dummy implementations for NRI hooks that are not used by this framework.
func (p *Plugin) CreateContainer(context.Context, *api.PodSandbox, *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	return nil, nil, nil
}
func (p *Plugin) StartContainer(context.Context, *api.PodSandbox, *api.Container) error { return nil }
func (p *Plugin) StopContainer(context.Context, *api.PodSandbox, *api.Container) ([]*api.ContainerUpdate, error) {
	return nil, nil
}
func (p *Plugin) UpdateContainer(context.Context, *api.PodSandbox, *api.Container) ([]*api.ContainerUpdate, error) {
	return nil, nil
}
func (p *Plugin) Shutdown(context.Context) { klog.Info("NRI plugin shutting down") }

// GetDevices returns a list of devices that the driver can manage. This is called
// periodically by the framework to publish the available resources.
func (p *Plugin) GetDevices() ([]resourceapi.Device, error) {
	return []resourceapi.Device{}, nil
}

// PrepareDevice is called by the framework during the NodePrepareResources hook.
// It should prepare the device for use by a pod and return any information that
// is needed by the NRI hooks. This information will be stored in the shared state.
func (p *Plugin) PrepareDevice(ctx context.Context, claim *resourceapi.ResourceClaim) (interface{}, error) {
	klog.V(2).Infof("PrepareDevice called for claim %s", claim.Name)
	p.mu.Lock()
	defer p.mu.Unlock()

	return nil, nil
}

// UnprepareDevice is called by the framework during the NodeUnprepareResources hook.
// It should clean up any resources that were allocated for the device.
func (p *Plugin) UnprepareDevice(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	klog.V(2).Infof("UnprepareDevice called for claim %s", claim.Name)
	return nil

}

// ConfigureDeviceForPod is called by the framework during the RunPodSandbox NRI hook.
// It should configure the device for use by the pod. The `preparedData` is the
// information that was returned by PrepareDevice.
func (p *Plugin) ConfigureDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	klog.V(2).Infof("ConfigureDeviceForPod called for device %s", device.Name)
	return nil
}

// CleanupDeviceForPod is called by the framework during the StopPodSandbox NRI hook.
// It should clean up any resources that were allocated for the device.
func (p *Plugin) CleanupDeviceForPod(device AllocatedDevice, networkNamespace string, podSandbox *api.PodSandbox, preparedData interface{}) error {
	klog.V(2).Infof("CleanupDeviceForPod called for device %s", device.Name)
	return nil
}
