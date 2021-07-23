# Gravity Enhancement Proposal

## GEP-001: Change DNS balancer to a software loadbalancer (HAProxy)

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
    - [Goals](#goals)
    - [Non-Goals](#non-goals)
- [Proposal](#proposal)
    - [User Stories (Optional)](#user-stories-optional)
        - [Story 1](#story-1)
        - [Story 2](#story-2)
    - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
    - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
    - [Test Plan](#test-plan)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
    - [Feature Enablement and Rollback](#feature-enablement-and-rollback)
- [Implementation History](#implementation-history)
\
<!-- /toc -->

## Summary
The main goal of this proposal is to replace dns balancing with a software loadbalancer.
Thus, all requests to the kube-apiserver from Kubernetes components (kubelet, scheduler, control-manager, kube-proxy)
will go through this loadbalancer. Also, requests to the docker registry must also go through the loadbalancer.

## Motivation
At the moment, Gravity is using CoreDNS to determine which master to send requests to,
but DNS is not a good tool for balancing traffic.
This proposal aims to provide the following improvements:
  1. will evenly distribute the load on the apiserver kubernetes
  1. Improve the stability and scalability of the cluster
  1. nodes will react faster on the disaster of the masters and recovery process
  1. the update will be faster and more efficient

### Goals
When creating a cluster, the user can choose which load balancer will be external or internal.
External loadbalancer is not controlled or configured by Gravity, it is the user's responsibility.
The internal loadbalancer, on the contrary, is controlled and configured by Gravity, the planet-agent is responsible for this.
When using an internal loadbalancer, a loadbalancer (HAProxy) will be installed for each node in the cluster including the master.
When changing the configuration of the masters, the agent should automatically change the loadbalancer configuration on each node.
All Kubernetes components (kubelet, scheduler, control-manager, kube-proxy) will use the loadbalancer to access the apiserver.

### Non-Goals

## Proposal
Change DNS balancer to a software loadbalancer (HAProxy).
CoreDNS will resolve the `leader.telekube.local` address, which will point either to the external address (domain name or IP) of the loadbalancer or to 127.0.0.1 when using the internal loadbalancer address.
Image names should remain unchanged and start with `leader.telekube.local:5000`.
To do this, the docker register must use port 5001, and port 5000 must be open on the loadbalancer.

### User Stories (Optional)
#### Story 1
As a cluster administrator, I can set the loadbalancer settings when creating a cluster. Set the type of loadbalancer external or internal and indicate the external address.
#### Story 2
As a cluster administrator, I can change the loadbalancer settings on a running cluster

### Risks and Mitigations
The `leader.telekube.local` address will not point to the master node.
If any applications use this address, they should be changed accordingly.

## Design Details
An additional section will be added to the cluster manifest for setting up the loadbalancer:
```yaml
kind: Cluster
apiVersion: cluster.gravitational.io/v2
loadbalancer:
  # default value is internal
  type: "external|internal"
  # gravity uses this field when type is external
  externalAddress: "IP address | DNS address"
```
The following section will be added to the cluster configuration:
```yaml
kind: ClusterConfiguration
version: v1
spec:
  loadbalancer:
    # default value is internal
    type: "external|internal"
    # gravity uses this field when type is external
    externalAddress: "IP address | DNS address"
```
The names of docker images should not be changed, and they should start with `leader.telekube.local:5000/`. 
So docker registers on master nodes by default will use port 5001 instead of 5000, 
and loadbalancer will use port 5000. The address `leader.telekube.local` will point to 127.0.0.1 
for the internal loadbalancer or the address of the external loadbalancer.

Kubeconfig files will use a new address to point to kube-apiserver, it will be 127.0.0.1 for the internal loadbalancer or the address of the external loadbalancer.

For an external load balancer:
The user must configure the balancer himself to open the following ports
  1. for kube-apiserver: 9443 -> (master ip addresses): 6443
  1. for docker-registry: 5000 -> (master ip addresses): 5001

For an internal load balancer:
The software load balancer is haproxy, it will run on each node in the cluster.
Port 9443 will be used for load balancing kube-apiserver and port 5000 will be used for load balancing in the docker registry.
The planet agent on master nodes saves and monitors the ip address in etcd using the key `/planet/cluster/${KUBE_CLUSTER_ID}/masters/${MASTER_IP}` with TTL 4 hours
The planet agent on each node, including the master is tracing all keys 
from etcd `/planet/cluster/${KUBE_CLUSTER_ID}/masters/` to change the haproxy configuration and reload it if necessary.

### Test Plan
- [ ] Install the cluster with the default configuration for the load balancer
- [ ] Install the cluster with the external load balancer
- [ ] Upgrade the cluster from a previous version to a version with an internal load balancer
- [ ] Upgrade a cluster from a previous version to a version with an external load balancer
- [ ] Change the type for the working cluster: internal(default) -> external -> internal

## Production Readiness Review Questionnaire
### Feature Enablement and Rollback
This feature will be enabled by default.
If the user wants to disable it, the cluster should be rolled back to an older version of Gravity.

## Implementation History
2021-07-21 Initial GEP merged
