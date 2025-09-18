# DRA VIP (Dynamic Resource Allocation for Virtual IP)

A Kubernetes Dynamic Resource Allocation (DRA) driver for managing Floating IPs (VIPs) that enable live pod migration with persistent network identity.

## Overview

This project implements a DRA driver that provides pods with persistent network identities through Floating IPs. This enables zero-downtime live migration of stateful Kubernetes pods while preserving active TCP connections using Multipath TCP (MPTCP) and container checkpoint/restore functionality.

## Project Structure

- `cmd/controller/`: Central DRA controller that manages ResourceClaim lifecycle and FloatingIP allocation
- `cmd/driver/`: Node-level DaemonSet driver that configures pod network namespaces
- `install.yaml`: Kubernetes manifests for RBAC and deployment
- `examples/`: Sample ResourceClaim configurations
- `Makefile`: Build and deployment automation
- `Dockerfile`: Multi-stage container build for both components


## Architecture

The driver implements a dual-component architecture:

### DRA Controller

- **ResourceClaim Management**: Monitors and processes FloatingIP resource requests
- **IP Pool Management**: Maintains pool of available Floating IPs and handles allocation
- **Status Updates**: Updates ResourceSlices with allocated IP information

### Node Driver  

- **Pod Network Setup**: Configures pod network namespace with FloatingIP
- **MPTCP Configuration**: Enables MPTCP and configures path management

## Usage

### 1. Deploy the DRA Driver

```bash
# Build and deploy both controller and driver
make images
kubectl apply -f install.yaml
```

### 2. Create a DeviceClass

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: floating-ip
spec:
  selectors:
  - cel:
      expression: device.driver == "dravip"
```

### 3. Request a Floating IP

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-app-fip
spec:
  devices:
    requests:
    - name: fip
      deviceClassName: floating-ip
---
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  resourceClaims:
  - name: floating-ip
    resourceClaimName: my-app-fip
  containers:
  - name: app
    image: nginx
```

## Development

### Running Tests

```bash
# Run all tests
go test ./...

# Run with coverage  
go test -cover ./...
```

## Requirements

- Kubernetes 1.31+ with DRA feature gate enabled
- Container runtime with checkpoint/restore support (containerd, CRI-O)
- MPTCP-enabled kernel (Linux 5.6+)
- Go 1.21+ for development

## Limitations

- **UDP Applications**: Stateful UDP applications may experience disruption during migration
- **Network Fabric**: Requires external network infrastructure to support FIP routing
- **Kernel Features**: MPTCP must be available in the target environment

## License

Apache License 2.0