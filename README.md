# PersonaStack for Omarchy

**A desktop client for PersonaStack on Omarchy. Early work in progress.**

This repository is building an Omarchy desktop application with the same user-visible functionality as the PersonaStack macOS app. The Electron shell and a small set of hosted-window bridges are implemented. The product is incomplete and not ready to install. A local Arch package recipe exists, but there are no release packages.

## Planned functionality

The parity target includes the hosted PersonaStack experience in a desktop window, persistent sign-in, native concern notifications, persona chat and activity pop-outs, local Codex and Claude configuration, and PersonaStack Desktop Control for the logged-in Omarchy computer. Desktop Control includes GUI actions, bounded file operations, and managed command sessions under the existing PersonaStack authorization and lease model.

PersonaStack services remain authoritative for sign-in, workspace and persona access, configuration, gateway routing, and hosted presentation. The desktop client will own its native windows, protected local credentials, and the bounded local executor.

## Omarchy and Cua status

Cua announced compositor-native background computer use for Omarchy Edge on September 25, 2026. Cua describes a synthetic Hyprland cursor that can route input to a target window while the person keeps using their own pointer. The announcement says stable-channel promotion and ARM production packaging are not confirmed, and that application-specific behavior still needs testing. Cua's [driver documentation](https://github.com/trycua/cua/blob/main/docs/content/docs/how-to-guides/driver/install.mdx) lists Hyprland/Omarchy support as experimental and treats the Hyprland plugin as a separate component. Installing the Cua driver alone does not enable that background-input path. See the [Omarchy announcement](https://github.com/trycua/cua/blob/main/blog/omarchy-cua-driver.md).

The desktop app's Omarchy compatibility, complete Cua tool coverage, real Secret Service behavior, authentication, and native window behavior have not been validated. This project will report support only after those checks pass on an actual Omarchy session. The current candidate profile is recorded as unverified in [`support-profile.json`](support-profile.json). A local Arch package recipe is in [`packaging/`](packaging/README.md). It is not an Omarchy support claim or a release package.

## Proposed implementation

The implementation uses an Electron shell and one Go companion. Electron owns the hosted windows and a finite, origin-checked native bridge. The shell has a persistent hosted window, tray residency, generation-bound exact-origin bridge admission, system-browser Google sign-in handoff, time-bounded organization SSO navigation, persona chat and stack/activity pop-outs, downloads, generic concern notifications, and hosted Local Session and Desktop Control bridges. Electron checks the registered main frame, origin, and document generation. The PersonaStack API remains responsible for user and workspace authorization.

The Go companion has a producer-owned API client and installation service. It uses Linux Secret Service without a plaintext fallback. `internal/desktoplifecycle` connects the pinned Cua runtime, API enrollment, workspace attachment and revocation, lock monitor, authorized executor, local file/process operations, and outbound Gateway transport. Gateway owns API session preparation and claim. Setup reports readiness only after the connection acknowledges a fresh heartbeat. Startup recovery restores an enrolled installation when an authorized relay is active. Idle relay resources stop only after an aggregate API read proves there are no active mappings. Explicit pause and resume commands persist the user's pause choice, report paused readiness, fence local execution, and require an unlocked session plus an active relay before resume. Disconnect asks for native confirmation, revokes every workspace mapping for this installation through the existing API client, and removes its keyring credential only after successful API revocation. A failed revoke leaves the installation credential available and execution paused for retry. The Linux tray displays Desktop Control status and exposes pause, resume, repair, and disconnect independently of a hosted workspace session. Pause can remain available from a live Gateway or cached active relay while API status is stale; Resume requires a fresh active-relay read. The tray distinguishes a saved user pause from an inactive or unavailable runtime, and reports a typed update-required state when the pinned Cua protocol or driver version is incompatible. A successful preparation or repair clears that status. Its setup action opens the hosted Desktop Control route. The executor starts fenced until Hyprland reports an unlocked session. The Cua and CLI installers, bounded file/process operations, and scope-bound Codex/Claude setup are implemented in source. Packaged builds support a user-controlled Launch at Login tray entry and a hidden background launch switch. Tray repair is implemented in source. The app has no automatic updater. A previously built package can be restored manually with Arch's `pacman -U`; this replaces package-owned files and leaves per-user browser, keyring, and CLI state outside the package. Package upgrade and rollback have not been acceptance-tested. A local Arch package recipe has passed a package build in a disposable Arch container using the pinned app source and exact API client snapshot. Package installation and Omarchy acceptance remain open. Real API, Secret Service, sign-in, CLI compatibility, Cua target-application behavior, Gateway deployment authorization, and Omarchy behavior remain unverified.

## Development status

The detailed parity inventory and implementation checklist are maintained with the PersonaStack engineering workspace. This public README describes the project's status and scope.

## Development

Use Node `24.21.0` with npm `11.19.0`. Run `npm ci` and `npm test` from the repository root. The Go companion uses Go `1.27.1`; it imports the producer-owned client from the private `personastack-api` repository. Building it requires authorized GitHub access. Set `GOPRIVATE=github.com/personastack` and use configured Git authentication. Keep credentials out of this repository and command arguments. Run `GOTOOLCHAIN=go1.27.1 go test -parallel 8 ./...` from `companion/`. These host-independent checks do not validate Omarchy, Hyprland, Cua, keyring, or graphical-session behavior.

## Security

The client must not put machine credentials in a web renderer, browser storage, URLs, process arguments, MCP payloads, or logs. The hosted app and API remain responsible for user, workspace, persona, and integration authorization. Local commands and files will be exposed only through PersonaStack's finite, authorized Desktop Control operations.

## Contributing

The remaining host-independent work is limited to gaps found in the full macOS parity inventory. The package has no automatic updater; manual package downgrade and its state-retention behavior are documented, but remain unverified. Native window, keyring, Cua, CLI, login, Gateway, lifecycle, and installed-package acceptance remain open until tested on the exact Omarchy profile.
