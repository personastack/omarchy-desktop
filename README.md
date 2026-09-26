# PersonaStack for Omarchy

**A desktop client for PersonaStack on Omarchy. Early work in progress.**

This repository is building an Omarchy desktop application with the same user-visible functionality as the PersonaStack macOS app. The Electron shell and a small set of hosted-window bridges are implemented. The product is incomplete, not packaged, and not ready to install. There are no release packages.

## Planned functionality

The parity target includes the hosted PersonaStack experience in a desktop window, persistent sign-in, native concern notifications, persona chat and activity pop-outs, local Codex and Claude configuration, and PersonaStack Desktop Control for the logged-in Omarchy computer. Desktop Control includes GUI actions, bounded file operations, and managed command sessions under the existing PersonaStack authorization and lease model.

PersonaStack services remain authoritative for sign-in, workspace and persona access, configuration, gateway routing, and hosted presentation. The desktop client will own its native windows, protected local credentials, and the bounded local executor.

## Omarchy and Cua status

Cua announced compositor-native background computer use for Omarchy Edge on September 25, 2026. Cua describes a synthetic Hyprland cursor that can route input to a target window while the person keeps using their own pointer. The announcement says stable-channel promotion and ARM production packaging are not confirmed, and that application-specific behavior still needs testing. Cua's [driver documentation](https://github.com/trycua/cua/blob/main/docs/content/docs/how-to-guides/driver/install.mdx) lists Hyprland/Omarchy support as experimental and treats the Hyprland plugin as a separate component. Installing the Cua driver alone does not enable that background-input path. See the [Omarchy announcement](https://github.com/trycua/cua/blob/main/blog/omarchy-cua-driver.md).

The desktop app's Omarchy compatibility, complete Cua tool coverage, real Secret Service behavior, authentication, and native window behavior have not been validated. This project will report support only after those checks pass on an actual Omarchy session. The current candidate profile is recorded as unverified in [`support-profile.json`](support-profile.json).

## Proposed implementation

The implementation uses an Electron shell and one Go companion. Electron owns the hosted windows and a finite, origin-checked native bridge. Go owns machine credentials, the outbound PersonaStack connection, Cua, local CLI setup, files, and processes as those runtime paths are implemented. The current shell has a persistent hosted window, tray residency, generation-bound exact-origin bridge admission, Google OAuth navigation/pop-ups, persona chat and stack/activity pop-outs, downloads, generic concern notifications, and the hosted Desktop Control `sync`, `state`, and `prepare` messages. Electron checks the registered main frame, origin, and document generation. The PersonaStack API remains responsible for user and workspace authorization.

The Go companion has a producer-owned API client and installation service. It uses Linux Secret Service without a plaintext fallback. Its Desktop Control bridge returns local installation identity and reports Linux Cua, native executor, and Gateway readiness as false until those owners work. Setup therefore remains blocked before enrollment is saved. `npm start` builds the companion and Electron starts it when the hosted main frame calls either native bridge. Local-session setup now has a login-shell capability probe, API-owned bundle validation, origin-scoped preference storage, a five-minute scope-bound pending manager, and a Linux installer for PersonaStack-owned Codex and Claude plugins. The installer uses each CLI's plugin manager and verifies owned source/cache readback with private file modes. The finite `personastackLocalSession` Electron bridge is connected to the companion protocol. Real API, keyring, sign-in, CLI compatibility, provider/MCP connectivity, and Omarchy behavior remain unverified. A packaged Linux artifact does not yet bundle the companion. Cua and remote file/process execution are not implemented.

## Development status

The detailed parity inventory and implementation checklist are maintained with the PersonaStack engineering workspace. This public README describes the project's status and scope.

## Development

Use Node `24.21.0` with npm `11.19.0`. Run `npm ci` and `npm test` from the repository root. The Go companion uses Go `1.27.1`; it imports the producer-owned client from the private `personastack-api` repository. Building it requires authorized GitHub access. Set `GOPRIVATE=github.com/personastack` and use configured Git authentication. Keep credentials out of this repository and command arguments. Run `GOTOOLCHAIN=go1.27.1 go test -parallel 8 ./...` from `companion/`. These host-independent checks do not validate Omarchy, Hyprland, Cua, keyring, or graphical-session behavior.

## Security

The client must not put machine credentials in a web renderer, browser storage, URLs, process arguments, MCP payloads, or logs. The hosted app and API remain responsible for user, workspace, persona, and integration authorization. Local commands and files will be exposed only through PersonaStack's finite, authorized Desktop Control operations.

## Contributing

The next milestone is to bundle the built companion in a Linux application artifact and exercise its authenticated bridge on Omarchy, then implement the gateway and remaining host-independent Desktop Control paths. Native window, keyring, Cua, login, and lifecycle acceptance remains open until tested on the exact Omarchy profile.
