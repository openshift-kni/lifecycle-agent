# FBC catalogs

Catalogs are generated at Konflux build time and must not be committed.

There are two catalogs on this branch:

- `5.0/` — 5.0 FBC (`lifecycle-agent-fbc-5-0`). Includes the released 4.22 bundles plus the 5.0 placeholder last bundle; Konflux fills that placeholder from `bundle.builds.in.yaml`.
- `4.22/` — 4.22 FBC (`lifecycle-agent-fbc-4-22`). This pipeline exists only to **release 4.22**. Released 4.22.0 and 4.22.1 are static (no placeholder). Static 5.0 bundles will be added here over time.

When a new 4.22 version is released, add it to **both** `catalog-template.in.yaml` files (`4.22/` and `5.0/`).

Generated files (`catalog-template.out.yaml`, `lifecycle-agent/catalog.json`) are listed in `.gitignore`.
