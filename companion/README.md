# Go companion

This module will own PersonaStack machine credentials, enrollment and gateway transport, Cua integration, local CLI setup, and bounded filesystem and process execution.

The companion runtime is not implemented yet. The first host-independent package strictly parses the existing local-session command envelope. It does not probe or modify CLI profiles. Host-independent typed contracts and protocol code may be built before the Omarchy feasibility gate. Native keyring, Cua, filesystem, process and graphical-session behavior remains gated on the exact target profile. Keep the API and gateway wire contracts imported from their producer-owned Go packages.

`go.mod` requires Go 1.27.0 and suggests Go 1.27.1. Go may use a newer installed toolchain, so future companion validation must explicitly select the pinned toolchain with `GOTOOLCHAIN=go1.27.1` rather than treating the `toolchain` directive as an upper bound.
