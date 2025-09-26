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
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestNewKindIPAMProvider_ValidCIDR(t *testing.T) {
	client := fake.NewSimpleClientset()
	cidr := "10.0.0.0/24"

	provider, err := NewKindIPAMProvider(client, cidr)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if provider == nil {
		t.Fatalf("expected provider, got nil")
	}
	if provider.prefix.String() != cidr {
		t.Errorf("expected prefix %s, got %s", cidr, provider.prefix.String())
	}
	if provider.kubeClient != client {
		t.Errorf("expected kubeClient to be set")
	}
	if provider.assignedIPs == nil {
		t.Errorf("expected assignedIPs map to be initialized")
	}
}

func TestNewKindIPAMProvider_InvalidCIDR(t *testing.T) {
	client := fake.NewSimpleClientset()
	invalidCIDRs := []string{
		"not-a-cidr",
		"10.0.0.0",
		"10.0.0.0/33",
		"",
	}

	for _, cidr := range invalidCIDRs {
		provider, err := NewKindIPAMProvider(client, cidr)
		if err == nil {
			t.Errorf("expected error for CIDR %q, got none", cidr)
		}
		if provider != nil {
			t.Errorf("expected nil provider for CIDR %q, got non-nil", cidr)
		}
	}
}

func TestKindIPAMProvider_AllocateRelease(t *testing.T) {
	client := fake.NewSimpleClientset()
	// kind provider allocates all addresses in the CIDR, including network and broadcast.
	cidr := "192.168.1.0/30" // Small CIDR for testing: .0, .1, .2, .3
	provider, err := NewKindIPAMProvider(client, cidr)
	if err != nil {
		t.Fatalf("failed to create provider: %v", err)
	}

	// 1. Allocate all available IPs
	ip1, err := provider.Allocate(nil, "") // .0
	if err != nil {
		t.Fatalf("expected to allocate an IP, got error: %v", err)
	}
	ip2, err := provider.Allocate(nil, "") // .1
	if err != nil {
		t.Fatalf("expected to allocate an IP, got error: %v", err)
	}
	ip3, err := provider.Allocate(nil, "") // .2
	if err != nil {
		t.Fatalf("expected to allocate an IP, got error: %v", err)
	}
	ip4, err := provider.Allocate(nil, "") // .3
	if err != nil {
		t.Fatalf("expected to allocate an IP, got error: %v", err)
	}

	if ip1.Address() != "192.168.1.0" {
		t.Errorf("expected IP 192.168.1.0, got %s", ip1.Address())
	}
	if ip2.Address() != "192.168.1.1" {
		t.Errorf("expected IP 192.168.1.1, got %s", ip2.Address())
	}
	if ip3.Address() != "192.168.1.2" {
		t.Errorf("expected IP 192.168.1.2, got %s", ip3.Address())
	}
	if ip4.Address() != "192.168.1.3" {
		t.Errorf("expected IP 192.168.1.3, got %s", ip4.Address())
	}

	if len(provider.assignedIPs) != 4 {
		t.Errorf("expected 4 assigned IPs, got %d", len(provider.assignedIPs))
	}

	// 2. Allocate another one, should be full
	_, err = provider.Allocate(nil, "")
	if err == nil {
		t.Fatalf("expected error when allocating from a full pool, got none")
	}

	// 3. Try to allocate an already allocated IP
	_, err = provider.Allocate(nil, "192.168.1.1")
	if err == nil {
		t.Fatalf("expected error when allocating an already allocated IP, got none")
	}

	// 4. Try to allocate an IP outside the CIDR
	_, err = provider.Allocate(nil, "10.0.0.1")
	if err == nil {
		t.Fatalf("expected error when allocating an IP outside the CIDR, got none")
	}

	// 5. Release an IP
	err = provider.Release(ip2) // release .1
	if err != nil {
		t.Fatalf("expected no error when releasing an IP, got %v", err)
	}
	if len(provider.assignedIPs) != 3 {
		t.Errorf("expected 3 assigned IPs after release, got %d", len(provider.assignedIPs))
	}
	if _, exists := provider.assignedIPs["192.168.1.1"]; exists {
		t.Errorf("expected IP 192.168.1.1 to be released")
	}

	// 6. Release an IP that wasn't allocated
	err = provider.Release(&KindIPAddress{address: "192.168.1.1"})
	if err != nil {
		t.Fatalf("expected no error when releasing a non-allocated IP, got %v", err)
	}

	// 7. Allocate again, should get the released IP
	ip5, err := provider.Allocate(nil, "")
	if err != nil {
		t.Fatalf("expected to allocate an IP after release, got error: %v", err)
	}
	// The allocator iterates from the beginning, so it should find .1 as free
	if ip5.Address() != "192.168.1.1" {
		t.Errorf("expected to get the released IP 192.168.1.1, got %s", ip5.Address())
	}
	if len(provider.assignedIPs) != 4 {
		t.Errorf("expected 4 assigned IPs, got %d", len(provider.assignedIPs))
	}
}
