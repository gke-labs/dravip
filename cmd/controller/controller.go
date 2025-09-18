package main

import (
	"context"

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
}

// NewController creates a new controller framework instance.
func NewController(controllerName string, kubeClient kubernetes.Interface, resourceInformer resourcev1informers.ResourceClaimInformer) *Controller {
	c := &Controller{
		controllerName:      controllerName,
		kubeClient:          kubeClient,
		resourceClaimLister: resourceInformer.Lister(),
		resourceClaimReady:  resourceInformer.Informer().HasSynced,
		queue:               workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: controllerName}),
	}

	resourceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
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
	// Optionally log success
}

func (c *Controller) sync(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	claim, err := c.resourceClaimLister.ResourceClaims(namespace).Get(name)
	if err != nil {
		// The ResourceClaim may have been deleted
		if apierrors.IsNotFound(err) {
			klog.Infof("ResourceClaim %s/%s has been deleted", namespace, name)
			return nil
		}
		return err
	}

	// Skip claims that don't have an allocation yet
	if claim.Status.Allocation == nil {
		return nil
	}

	// Check if this claim has device requests we can handle
	var deviceClassName string
	if len(claim.Spec.Devices.Requests) > 0 {
		// Get the device class from the first device request
		if claim.Spec.Devices.Requests[0].Exactly != nil {
			deviceClassName = claim.Spec.Devices.Requests[0].Exactly.DeviceClassName
		} else {
			// FirstAvailable requests - check the first one
			if len(claim.Spec.Devices.Requests[0].FirstAvailable) > 0 {
				deviceClassName = claim.Spec.Devices.Requests[0].FirstAvailable[0].DeviceClassName
			} else {
				return nil // No device class specified
			}
		}
	} else {
		// No device requests, this claim is not for us
		return nil
	}

	klog.Infof("Reconciling ResourceClaim %s/%s with DeviceClass %s", claim.Namespace, claim.Name, deviceClassName)

	return nil
}
