# FBC catalogs

Catalogs are generated at Konflux build time and must not be committed.

There is one catalog on this branch:

- `4.23/` — 4.23 FBC (`lifecycle-agent-fbc-4-23`). Includes the released 4.22 bundles plus the 4.23 placeholder last bundle; Konflux fills that placeholder from `bundle.builds.in.yaml`.

Generated files (`catalog-template.out.yaml`, `lifecycle-agent/catalog.json`) are listed in `.gitignore`.
