# Go companion

This module will own PersonaStack machine credentials, enrollment and gateway transport, Cua integration, local CLI setup, and bounded filesystem and process execution.

The companion runtime is not implemented yet. Host-independent packages currently parse the existing local-session command envelope and store API-issued Desktop Control installation records through Linux Secret Service using `secret-tool`. The keyring adapter is tested with a fake command. It has no plaintext fallback. Real Secret Service behavior, enrollment, and credential lifecycle remain unverified until acceptance on the exact Omarchy profile. The local-session parser does not probe or modify CLI profiles. Keep API and gateway wire contracts imported from their producer-owned Go packages.

`go.mod` requires Go 1.27.0 and suggests Go 1.27.1. Go may use a newer installed toolchain, so future companion validation must explicitly select the pinned toolchain with `GOTOOLCHAIN=go1.27.1` rather than treating the `toolchain` directive as an upper bound.
