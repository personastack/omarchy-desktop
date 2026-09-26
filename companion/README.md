# Go companion

This module will own PersonaStack machine credentials, enrollment and gateway transport, Cua integration, local CLI setup, and bounded filesystem and process execution.

`cmd/personastack-companion` runs the Go companion as a newline-delimited JSON process. It accepts a single production or LAN app origin argument. The Electron bridge currently supports `sync`, `state`, and `prepare` requests. `prepare` reports that the Linux runtime is unavailable, so the companion does not consume an enrollment ticket or claim Desktop Control readiness. Replies contain only generic error codes, local installation status, and finite readiness flags. Machine credentials stay in the Linux Secret Service adapter and never appear in process arguments, protocol messages, or replies. `npm start` builds it into `companion/bin` and Electron starts it lazily for a request from the registered main frame at the configured origin and current document generation. Packaged app bundling is not configured yet.

Host-independent packages also parse the existing local-session command envelope and orchestrate readiness, session generation prepare/claim/heartbeat, and session readiness through the producer-owned API client. Tests use fake APIs and commands. There is no plaintext credential fallback. These tests do not prove real API or Secret Service behavior on Omarchy. The local-session parser does not probe or modify CLI profiles. Keep API and gateway wire contracts imported from their producer-owned Go packages.

`go.mod` requires Go 1.27.0 and suggests Go 1.27.1. Go may use a newer installed toolchain, so future companion validation must explicitly select the pinned toolchain with `GOTOOLCHAIN=go1.27.1` rather than treating the `toolchain` directive as an upper bound.
