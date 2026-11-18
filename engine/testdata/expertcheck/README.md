# compa expert check fixtures

Synthetic packages for `compa expert check` (`docs/EXPERT_API.md` §Checking packages), each root one scenario, used beside `testdata/catalogs/`. They carry no
customer data.

- `broken/`: a valid expert, `good`, and one package for each loader breach — a
  malformed manifest, an unknown primitive, a primitive parameter it does not
  take (found by the stage check, not the schema), an undeclared binary, a path
  escaping the package and a parameter spliced into an argument.
- `unexplained/`: an expert whose analyst text does not name a tool it uses.
- `unmet/`: an expert whose catalog range no installed version meets.
- `clash/`: an expert with the ID of the catalog `fixture.core`.
