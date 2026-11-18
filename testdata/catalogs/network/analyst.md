# Network (fixture)

## node-ready

`fixture.core.node_ready`: a node that is not Ready drops its pods' endpoints;
expect service disruption before any network fault.

## pods

`fixture.core.pods` in `openshift-ovn-kubernetes`: restarts of ovnkube pods
point at the node or the OVN databases.
