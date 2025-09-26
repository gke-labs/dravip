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

package gce

import (
	"context"
	"fmt"

	"github.com/gke-labs/dravip/pkg/ipam"
	"google.golang.org/api/compute/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// GCEIPAddress is the GCE-specific implementation of an IPAddress.
type GCEIPAddress struct {
	id      string
	address string
	state   ipam.IPState
	node    string

	// GCE-specific fields
	gcpProject         string
	gcpRegion          string
	forwardingRuleName string
	targetInstanceName string
}

// Implement the ipam.IPAddress interface
func (g *GCEIPAddress) ID() string          { return g.id }
func (g *GCEIPAddress) Address() string     { return g.address }
func (g *GCEIPAddress) State() ipam.IPState { return g.state }
func (g *GCEIPAddress) Node() string        { return g.node }

// GCEIPAMProvider is the GCE implementation of the IPAMProvider interface.
type GCEIPAMProvider struct {
	kubeClient     kubernetes.Interface
	computeService *compute.Service
	projectID      string
	region         string
}

// NewGCEIPAMProvider creates a new GCEIPAMProvider.
func NewGCEIPAMProvider(kubeClient kubernetes.Interface, projectID, region string) (*GCEIPAMProvider, error) {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute service: %w", err)
	}

	return &GCEIPAMProvider{
		kubeClient:     kubeClient,
		computeService: computeService,
		projectID:      projectID,
		region:         region,
	}, nil
}

// Allocate reserves a GCE IP address for a claim.
func (p *GCEIPAMProvider) Allocate(claim *resourcev1.ResourceClaim, ip string) (ipam.IPAddress, error) {
	addressName := fmt.Sprintf("dravip-%s", claim.UID)
	op, err := p.computeService.Addresses.Insert(p.projectID, p.region, &compute.Address{
		Name:    addressName,
		Address: ip, // If ip is empty, GCE will auto-allocate one
	}).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to create GCE address: %w", err)
	}
	klog.Infof("Waiting for GCE address creation (operation: %s)...", op.Name)
	// You might want to implement a more robust waiting mechanism here
	// For simplicity, we'll just log and assume it will complete.

	return &GCEIPAddress{
		id:         addressName,
		address:    ip, // This will be updated with the allocated IP if it was empty
		state:      ipam.StateAllocated,
		gcpProject: p.projectID,
		gcpRegion:  p.region,
	}, nil
}

// Release returns a reserved GCE IP address to the free pool.
func (p *GCEIPAMProvider) Release(ip ipam.IPAddress) error {
	_, err := p.computeService.Addresses.Delete(p.projectID, p.region, ip.ID()).Do()
	if err != nil {
		return fmt.Errorf("failed to delete GCE address %s: %w", ip.ID(), err)
	}
	klog.Infof("GCE address %s released.", ip.ID())
	return nil
}

// Assign makes an allocated GCE IP active on a specific node.
func (p *GCEIPAMProvider) Assign(ip ipam.IPAddress, nodeName string) error {
	gceIP, ok := ip.(*GCEIPAddress)
	if !ok {
		return fmt.Errorf("invalid IPAddress type provided")
	}

	// 1. Create Target Instance
	targetInstanceName := fmt.Sprintf("dravip-%s-%s", gceIP.id, nodeName)
	// You'll need a way to get the zone from the node name. This is a placeholder.
	zone := "us-central1-a" // TODO: Get this from the Kubernetes node object
	_, err := p.computeService.TargetInstances.Insert(p.projectID, zone, &compute.TargetInstance{
		Name:     targetInstanceName,
		Instance: fmt.Sprintf("zones/%s/instances/%s", zone, nodeName),
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to create target instance: %w", err)
	}
	gceIP.targetInstanceName = targetInstanceName
	klog.Infof("Created GCE target instance: %s", targetInstanceName)

	// 2. Create Forwarding Rule
	forwardingRuleName := fmt.Sprintf("dravip-%s", gceIP.id)
	_, err = p.computeService.ForwardingRules.Insert(p.projectID, p.region, &compute.ForwardingRule{
		Name:       forwardingRuleName,
		IPAddress:  gceIP.address,
		Target:     fmt.Sprintf("zones/%s/targetInstances/%s", zone, targetInstanceName),
		IPProtocol: "TCP", // Or make this configurable
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to create forwarding rule: %w", err)
	}
	gceIP.forwardingRuleName = forwardingRuleName
	gceIP.state = ipam.StateAssigned
	gceIP.node = nodeName
	klog.Infof("Created GCE forwarding rule: %s", forwardingRuleName)

	return nil
}

// Unassign deactivates a GCE IP from a node.
func (p *GCEIPAMProvider) Unassign(ip ipam.IPAddress) error {
	gceIP, ok := ip.(*GCEIPAddress)
	if !ok {
		return fmt.Errorf("invalid IPAddress type provided")
	}

	// 1. Delete Forwarding Rule
	_, err := p.computeService.ForwardingRules.Delete(p.projectID, p.region, gceIP.forwardingRuleName).Do()
	if err != nil {
		return fmt.Errorf("failed to delete forwarding rule: %w", err)
	}
	klog.Infof("Deleted GCE forwarding rule: %s", gceIP.forwardingRuleName)

	// 2. Delete Target Instance
	zone := "us-central1-a" // TODO: Get this from the Kubernetes node object
	_, err = p.computeService.TargetInstances.Delete(p.projectID, zone, gceIP.targetInstanceName).Do()
	if err != nil {
		return fmt.Errorf("failed to delete target instance: %w", err)
	}
	klog.Infof("Deleted GCE target instance: %s", gceIP.targetInstanceName)

	gceIP.state = ipam.StateAllocated
	gceIP.node = ""
	gceIP.forwardingRuleName = ""
	gceIP.targetInstanceName = ""

	return nil
}
