# Go companion

This module will own PersonaStack machine credentials, enrollment and gateway transport, Cua integration, local CLI setup, and bounded filesystem and process execution.

The companion runtime is intentionally not implemented yet. Add its first package only after the active plan's native Omarchy feasibility gate passes. Keep the API and gateway wire contracts imported from their producer-owned Go packages.

`go.mod` requires Go 1.27.0 and suggests Go 1.27.1. Go may use a newer installed toolchain, so future companion validation must explicitly select the pinned toolchain with `GOTOOLCHAIN=go1.27.1` rather than treating the `toolchain` directive as an upper bound.
