# Storage (fixture)

A volume that will not attach is often a node problem first.

## node-ready

`fixture.core.node_ready`: a node that is not Ready cannot attach volumes; check
it before the CSI driver.

## degraded-operators

`fixture.core.degraded_operators`: a degraded storage operator is a symptom;
relate it to the CSI driver's pods.

## node-files

Which nodes the must-gather captured at all.
