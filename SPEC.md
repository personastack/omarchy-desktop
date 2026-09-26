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

The current Electron shell uses a persistent, origin-specific browser partition. It disables renderer Node integration, enables context isolation and sandboxing, limits Google OAuth navigation/pop-ups to Google and the configured app origin, and admits native bridge calls only from registered current main frames at the exact configured origin and document generation. Other external main-frame navigation is denied. Hosted same-product navigation remains in-app without retaining bridge privileges on a different origin. User-activated HTTP(S) links open in the system browser. Concern, chat, chat-window, stack, Local Session, and Desktop Control bridge names are exposed. Hosted Desktop Control supports `sync`, `state`, and `prepare`; the reserved `desktop:lifecycle` scope is only available to the native tray path. The Linux tray reads app-wide lifecycle state and exposes pause/resume through a separate finite companion command path without changing or depending on hosted workspace scope. Only this tray state path reads current API relay activity. Hosted local installation state remains available during relay-status outages. The tray distinguishes a user's saved pause from an inactive relay, reports unavailable native runtime separately, disables Resume and setup after a failed or pending status read, and preserves Pause when a live Gateway or cached active relay allows a safe local fence. It retains lifecycle action errors through status polling. The setup action opens `/user/desktop-control` in the main window. Local Session supports the hosted versioned `state`, `select_harness`, `prepare`, and `configure` messages through the Go companion, bounded login-shell probes, API-owned bundle validation, and a PersonaStack-owned Linux plugin installer. `internal/desktoplifecycle` owns setup-time Cua preparation, API enrollment/attachment, Gateway connection and reconnect, executor startup, startup recovery, aggregate relay cleanup, explicit pause/resume persistence, and the Hyprland lock monitor. Agent Gateway owns API session preparation and claim during the authenticated WebSocket handshake. Setup waits for a fresh heartbeat acknowledgement before reporting Gateway readiness. The executor starts with unknown lock state and rejects commands until the lock monitor observes an unlocked session. `internal/hyprlandlock` queries `hyprctl -j locked` and fences the executor to unknown on read failures. `internal/desktopexecutor` contains finite Cua dispatch, bounded screenshot results, scoped lease/revocation policy, and the lock fence. `internal/desktoplocal` connects the bounded Linux file and process managers to the executor. Desktop Control `prepare` reports native runtime readiness. An unconfigured setup session can remain alive for ten minutes while the first workspace config is saved. Repair/disconnect tray actions and the complete native window-control adapter remain unimplemented. Electron frame and origin checks admit bridge calls; PersonaStack API remains the authority for user and workspace authorization. These source-level controls do not establish native session or sign-in acceptance, real CLI compatibility, Gateway authorization in deployment, Cua application support, or Omarchy behavior.

## Parity outcome

Implement the required journeys in the active plan: hosted app/login and notifications; chat, stack and activity windows; Codex and Claude setup; installation enrollment and status; all reviewed GUI actions; bounded files and managed processes; lock, revocation, reconnect, packaging and lifecycle. Completion requires native evidence for the advertised Omarchy profile. A passing source test or tool response alone does not certify native behavior.
