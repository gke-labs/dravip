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

package ipam

import (
	resourcev1 "k8s.io/api/resource/v1"
)

// IPState represents the lifecycle of an IP address.
type IPState string

const (
	// StateFree indicates that the IP is available for allocation.
	StateFree IPState = "Free"
	// StateAllocated indicates that the IP has been reserved for a ResourceClaim.
	StateAllocated IPState = "Allocated"
	// StateAssigned indicates that the IP is actively assigned to a node.
	StateAssigned IPState = "Assigned"
)

// IPAddress represents a single IP address and its state.
type IPAddress interface {
	// ID returns the unique identifier for this IP address.
	ID() string
	// Address returns the actual IP address string.
	Address() string
	// State returns the current state of the IP address.
	State() IPState
	// Node returns the name of the node the IP is programmed on, if any.
	Node() string
}

// IPAMProvider defines the contract for any IP address provider.
type IPAMProvider interface {
	// Allocate reserves an IP for a claim. It can be a specific IP or a random one.
	Allocate(claim *resourcev1.ResourceClaim, ip string) (IPAddress, error)
	// Release returns a reserved IP to the free pool.
	Release(ip IPAddress) error
	// Assign makes an allocated IP active on a specific node.
	Assign(ip IPAddress, nodeName string) error
	// Unassign deactivates an IP from a node without releasing it.
	Unassign(ip IPAddress) error
}
