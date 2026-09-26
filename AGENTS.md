# Omarchy Desktop repository instructions

Read `SPEC.md` and the active workspace implementation plan before changing the desktop contract. The macOS desktop app, API clients, hosted UI and MCP catalog remain the behavioral and contract authorities.

## Current milestone

The project is a public work in progress. Eric authorized host-independent implementation before the Omarchy feasibility gate is closed. Keep all native acceptance checks open and do not claim the app or any feature is supported until tested on the exact Omarchy profile. Cua Omarchy support remains experimental and may require a separately installed Hyprland input plugin. Do not change the user's Omarchy channel or restart Hyprland for tests.

## Implementation rules

- Keep the hosted PersonaStack app and API as authorities for sign-in, authorization, product state and remote Desktop Control grants.
- Keep remote pages sandboxed. Do not expose Node, generic IPC, machine credentials, filesystem APIs or arbitrary command execution to a web renderer.
- Keep the Electron/Go boundary finite and typed. Do not add a localhost service, detached daemon or second auth store.
- Use producer-owned Go client packages for API and gateway contracts. Do not copy their DTOs.
- Pin Electron, Cua Driver, the Hyprland plugin, Go modules and package artifacts. Record checksums for native downloads.
- Scope credential handling to a protected Linux keyring and follow the current security behavior in `SPEC.md` and the parity plan.
- Preserve the existing macOS and ordinary-browser journeys when changing shared PersonaStack contracts.

## Validation

- Use `npm ci` from the repository root to restore the exact npm dependency tree.
- For TypeScript changes, use the repository's TypeScript LSP before source searches and run focused type checks and tests.
- For Go changes, use `gopls` before source searches. Run focused tests in the owning Go package with parallelism set to the available cores.
- Native Cua, Hyprland window, Secret Service and graphical-session claims require acceptance on the exact supported Omarchy profile. Mocks and WSL do not prove those claims.
- Keep credentials out of logs, renderer state, command arguments, MCP content and committed files.
