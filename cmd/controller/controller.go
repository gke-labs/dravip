/*
Copyright 2025 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"

	"github.com/gke-labs/dravip/pkg/ipam"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	resourcev1informers "k8s.io/client-go/informers/resource/v1"
	"k8s.io/client-go/kubernetes"
	resourcev1lister "k8s.io/client-go/listers/resource/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// Controller manages the lifecycle of the Kubernetes controller.
type Controller struct {
	controllerName      string
	kubeClient          kubernetes.Interface
	resourceClaimLister resourcev1lister.ResourceClaimLister
	resourceClaimReady  cache.InformerSynced
	queue               workqueue.TypedRateLimitingInterface[string]
	ipamProvider        ipam.IPAMProvider
}

// NewController creates a new controller framework instance.
func NewController(
	controllerName string,
	kubeClient kubernetes.Interface,
	resourceInformer resourcev1informers.ResourceClaimInformer,
	provider ipam.IPAMProvider,
) *Controller {
	c := &Controller{
		controllerName:      controllerName,
		kubeClient:          kubeClient,
		resourceClaimLister: resourceInformer.Lister(),
		resourceClaimReady:  resourceInformer.Informer().HasSynced,
		queue:               workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: controllerName}),
		ipamProvider:        provider,
	}

	_, _ = resourceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
	})

	return c
}

// Run starts the controller's reconciliation loop.
func (c *Controller) Run(ctx context.Context) {
	klog.Infof("Starting controller: %s", c.controllerName)
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(ctx.Done(), c.resourceClaimReady) {
		klog.Fatalf("Failed to sync cache for controller: %s", c.controllerName)
	}

	// Start worker(s)
	go c.runWorker(ctx)

	<-ctx.Done()
	klog.Infof("Shutting down controller: %s", c.controllerName)
}

func (c *Controller) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			c.processNextWorkItem(ctx)
		}
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) {
	key, shutdown := c.queue.Get()
	if shutdown {
		return
	}
	defer c.queue.Done(key)

	err := c.sync(ctx, key)
	if err != nil {
		c.queue.AddRateLimited(key)
		klog.Errorf("Error syncing %q: %v, requeuing", key, err)
		return
	}
	c.queue.Forget(key)
}

func (c *Controller) sync(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	claim, err := c.resourceClaimLister.ResourceClaims(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("ResourceClaim %s/%s has been deleted", namespace, name)
			// TODO: Implement proper cleanup logic by storing allocated IPs
			return nil
		}
		return err
	}

	// If the claim is being deleted, release the IP
	if !claim.DeletionTimestamp.IsZero() {
		// TODO: Retrieve the IP associated with this claim and release it
		// ip, err := getIPForClaim(claim)
		// if err := c.ipamProvider.Release(ip); err != nil { ... }
		return nil
	}

	// If claim is not allocated yet, try to allocate an IP
	if claim.Status.Allocation == nil {
		// For now, we assume any claim with our device class is for us
		// A more robust implementation would check the DeviceClassName
		ip, err := c.ipamProvider.Allocate(claim, "") // Allocate a dynamic IP
		if err != nil {
			return fmt.Errorf("failed to allocate IP for claim %s: %w", key, err)
		}
		klog.Infof("Allocated IP %s for claim %s", ip.Address(), key)
		// TODO: Update ResourceSlice with the new IP
		return nil
	}

	// If claim IS allocated, assign the IP to the node
	if claim.Status.Allocation != nil {
		nodeName := "" // TODO: Figure out how to get the node name from the allocation result
		if nodeName == "" {
			klog.Warningf("Could not determine node for claim %s, skipping assignment", key)
			return nil
		}

		// TODO: Retrieve the IP associated with this claim
		// ip, err := getIPForClaim(claim)
		// if err := c.ipamProvider.Assign(ip, nodeName); err != nil { ... }
		klog.Infof("Assigned IP to node %s for claim %s", nodeName, key)
	}

	return nil
}
