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

package kind

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"sync"

	"github.com/gke-labs/dravip/pkg/ipam"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// KindIPAddress is the in-memory implementation of an IPAddress for KIND.
type KindIPAddress struct {
	id      string
	address string
	state   ipam.IPState
	node    string
}

// Implement the ipam.IPAddress interface
func (k *KindIPAddress) ID() string          { return k.id }
func (k *KindIPAddress) Address() string     { return k.address }
func (k *KindIPAddress) State() ipam.IPState { return k.state }
func (k *KindIPAddress) Node() string        { return k.node }

// KindIPAMProvider is the simple in-memory implementation for KIND.
type KindIPAMProvider struct {
	kubeClient  kubernetes.Interface
	prefix      netip.Prefix
	assignedIPs map[string]*KindIPAddress // Only track assigned IPs
	mu          sync.Mutex
}

// NewKindIPAMProvider creates a new KindIPAMProvider.
func NewKindIPAMProvider(kubeClient kubernetes.Interface, cidrStr string) (*KindIPAMProvider, error) {
	prefix, err := netip.ParsePrefix(cidrStr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidrStr, err)
	}

	return &KindIPAMProvider{
		kubeClient:  kubeClient,
		prefix:      prefix,
		assignedIPs: make(map[string]*KindIPAddress),
	}, nil
}

// Allocate reserves an IP from the pool.
func (p *KindIPAMProvider) Allocate(claim *resourcev1.ResourceClaim, ip string) (ipam.IPAddress, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ip != "" {
		// Allocate a specific, requested IP
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %q: %w", ip, err)
		}
		if !p.prefix.Contains(addr) {
			return nil, fmt.Errorf("ip %s is not in the configured CIDR %s", ip, p.prefix)
		}
		if _, exists := p.assignedIPs[ip]; exists {
			return nil, fmt.Errorf("ip %s is already allocated", ip)
		}

		newIP := &KindIPAddress{id: ip, address: ip, state: ipam.StateAllocated}
		p.assignedIPs[ip] = newIP
		return newIP, nil
	}

	// Allocate the next available IP by iterating from the start
	iterAddr := p.prefix.Addr()
	for {
		// Stop if we've iterated past the end of the subnet
		if !p.prefix.Contains(iterAddr) {
			break
		}
		ipStr := iterAddr.String()
		if _, exists := p.assignedIPs[ipStr]; !exists {
			// Found the first available IP
			newIP := &KindIPAddress{id: ipStr, address: ipStr, state: ipam.StateAllocated}
			p.assignedIPs[ipStr] = newIP
			return newIP, nil
		}
		iterAddr = iterAddr.Next()
	}

	return nil, fmt.Errorf("no free IPs available in CIDR %s", p.prefix)
}

// Release removes an IP from the assigned pool.
func (p *KindIPAMProvider) Release(ip ipam.IPAddress) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.assignedIPs[ip.Address()]; !exists {
		klog.Warningf("Attempted to release IP %s which was not allocated", ip.Address())
		return nil // Releasing a non-allocated IP is a no-op
	}
	delete(p.assignedIPs, ip.Address())
	klog.Infof("Released IP %s", ip.Address())
	return nil
}

// Assign adds a route on the host to the specified KIND node container.
func (p *KindIPAMProvider) Assign(ip ipam.IPAddress, nodeName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	kindIP, exists := p.assignedIPs[ip.Address()]
	if !exists || kindIP.state != ipam.StateAllocated {
		return fmt.Errorf("ip %s is not in an allocatable state", ip.Address())
	}

	// Get the node's internal IP address (this is the Docker container IP)
	node, err := p.kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	nodeIP := ""
	for _, addr := range node.Status.Addresses {
		if addr.Type == v1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}
	if nodeIP == "" {
		return fmt.Errorf("could not find internal IP for node %s", nodeName)
	}

	// Use "ip route add" to program the route on the host
	klog.Infof("Assigning IP %s to node %s by adding route: 'ip route add %s via %s'", ip.Address(), nodeName, ip.Address(), nodeIP)
	cmd := exec.Command("ip", "route", "add", ip.Address(), "via", nodeIP)
	if err := cmd.Run(); err != nil {
		// This might fail if the route already exists, which can be okay.
		klog.Warningf("Could not add route for %s (it may already exist): %v", ip.Address(), err)
	}

	kindIP.state = ipam.StateAssigned
	kindIP.node = nodeName
	return nil
}

// Unassign removes the route from the host.
func (p *KindIPAMProvider) Unassign(ip ipam.IPAddress) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	kindIP, exists := p.assignedIPs[ip.Address()]
	if !exists || kindIP.state != ipam.StateAssigned {
		return fmt.Errorf("ip %s is not in an assigned state", ip.Address())
	}

	// Use "ip route del" to remove the route
	klog.Infof("Unassigning IP %s from node %s by deleting route", ip.Address(), kindIP.node)
	cmd := exec.Command("ip", "route", "del", ip.Address())
	if err := cmd.Run(); err != nil {
		// It's often okay if the route is already gone, so we just log this.
		klog.Warningf("Failed to delete route for %s (this may be expected): %v", ip.Address(), err)
	}

	kindIP.state = ipam.StateAllocated
	kindIP.node = ""
	return nil
}
