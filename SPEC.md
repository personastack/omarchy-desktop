# PersonaStack for Omarchy specification

## Purpose

Provide a desktop client for PersonaStack on Omarchy with the complete user-visible functionality of `macos-desktop`.

The Electron shell and initial chat/stack bridges exist. The app is incomplete and no Omarchy profile or feature is certified. The full required behavior and completion evidence live in the active workspace plan. This spec records the repository's ownership boundaries and current desired outcome.

## Authority

- `personastack-api` owns identity, authorization, workspaces, personas, product state, enrollment and revocation.
- `my.personastack.ai` owns authenticated hosted pages, OAuth callbacks, browser-facing composition, notifications event data and pop-out routes.
- `mcp` and `agent-gateway` own reviewed Desktop Control tools, typed request contracts, transport and routing.
- This client will own Linux window presentation, the finite native bridge, local harness setup, protected machine credential custody, outbound connection lifecycle, and bounded GUI, filesystem and process execution.
- Hyprland and Cua own their compositor and device-control interfaces. This application must use a pinned supported driver/plugin contract and must not claim support for behavior that the tested target profile cannot provide.

The client must not create a second login, authorization service, integration store, product database, realtime event authority, MCP catalog or gateway protocol.

## Supported environment

No environment is currently supported. The first candidate is Omarchy Edge on x86_64 with an exact Omarchy, Hyprland, Electron, Cua Driver, Cua Hyprland input plugin and Secret Service profile recorded by the feasibility gate. ARM64, other Linux distributions, other compositors, stable-channel Omarchy and unsupported Cua applications are outside the initial claim until separately proven.

## Security boundary

Remote content receives only finite presentation messages from registered top-level windows at the configured PersonaStack app origin. Machine credentials stay in protected OS storage and native process memory. The API authorizes each workspace, persona and Desktop Control operation. Native execution accepts only reviewed typed operations under the current lease and scope. A failed or uncertain mutation is not replayed automatically.

The current Electron shell uses a persistent, origin-specific browser partition. It disables renderer Node integration, enables context isolation and sandboxing, limits Google OAuth navigation/pop-ups to Google and the configured app origin, and admits native bridge calls only from registered current main frames at the exact configured origin and document generation. Other external main-frame navigation is denied. Hosted same-product navigation remains in-app without retaining bridge privileges on a different origin. User-activated HTTP(S) links open in the system browser. Concern, chat, chat-window, stack, Local Session, and Desktop Control bridge names are exposed. Desktop Control supports the hosted `sync`, `state`, and `prepare` messages. Local Session supports the hosted versioned `state`, `select_harness`, `prepare`, and `configure` messages through the Go companion, bounded login-shell probes, API-owned bundle validation, and a PersonaStack-owned Linux plugin installer. `internal/desktopexecutor` contains the finite Cua dispatcher, bounded screenshot results, scoped lease/revocation policy, and a fail-closed session lock fence. `internal/hyprlandlock` can query `hyprctl -j locked` and report unknown on read failures, but the monitor is not yet wired into companion startup or the executor lifecycle. `internal/desktopfiles` and `internal/desktopprocess` contain standalone bounded Linux operation managers, but neither is yet connected to the executor or Gateway. The Desktop Control bridge still reports the runtime unavailable, and Cua, native executor, and Gateway readiness remain false. Electron frame and origin checks admit the bridge call; PersonaStack API remains the authority for user and workspace authorization. These source-level controls do not establish native session or sign-in acceptance or real CLI compatibility.

## Parity outcome

Implement the required journeys in the active plan: hosted app/login and notifications; chat, stack and activity windows; Codex and Claude setup; installation enrollment and status; all reviewed GUI actions; bounded files and managed processes; lock, revocation, reconnect, packaging and lifecycle. Completion requires native evidence for the advertised Omarchy profile. A passing source test or tool response alone does not certify native behavior.
