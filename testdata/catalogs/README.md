# Catalog fixtures

Synthetic packages for shared tool catalogs (`docs/EXPERT_API.md` §Shared tool
catalog): the catalog `fixture.core` at 0.1.0, 0.1.1 and 0.2.0 (0.2.0 renames
`node_ready`'s parameter, a breaking change), and two v1alpha3 experts using it
with different ranges — `fixture.storage` (`<0.2.0`, with a test of its own) and
`fixture.network` (`>=0.2.0 <0.3.0`). They carry no customer data. The experts in
`testdata/experts/`, which use no catalog, are kept apart, so the goldens built
from them change only when they do.
