# PersonaStack for Omarchy

**A desktop client for PersonaStack on Omarchy. Work in progress.**

This repository is for building an Omarchy desktop application with the same user-visible functionality as the PersonaStack macOS app. The app is not implemented or ready to install yet. There are no release packages.

## Planned functionality

The parity target includes the hosted PersonaStack experience in a desktop window, persistent sign-in, native concern notifications, persona chat and activity pop-outs, local Codex and Claude configuration, and PersonaStack Desktop Control for the logged-in Omarchy computer. Desktop Control includes GUI actions, bounded file operations, and managed command sessions under the existing PersonaStack authorization and lease model.

PersonaStack services remain authoritative for sign-in, workspace and persona access, configuration, gateway routing, and hosted presentation. The desktop client will own its native windows, protected local credentials, and the bounded local executor.

## Omarchy and Cua status

Cua announced compositor-native background computer use for Omarchy Edge on September 25, 2026. Cua describes a synthetic Hyprland cursor that can route input to a target window while the person keeps using their own pointer. The announcement says stable-channel promotion and ARM production packaging are not confirmed, and that application-specific behavior still needs testing. Cua's [driver documentation](https://github.com/trycua/cua/blob/main/docs/content/docs/how-to-guides/driver/install.mdx) lists Hyprland/Omarchy support as experimental and treats the Hyprland plugin as a separate component. Installing the Cua driver alone does not enable that background-input path. See the [Omarchy announcement](https://github.com/trycua/cua/blob/main/blog/omarchy-cua-driver.md).

The desktop app's Omarchy compatibility, complete Cua tool coverage, secure keyring integration, and native window behavior have not been validated. This project will report support only after those checks pass on an actual Omarchy session.

## Proposed implementation

The implementation plan proposes an Electron shell and one Go companion. Electron would own the hosted windows and a finite, origin-checked native bridge. Go would own machine credentials, the outbound PersonaStack connection, Cua, local CLI setup, files, and processes. This design is subject to native Omarchy feasibility checks before the application is built.

## Development status

The detailed parity inventory and implementation checklist are maintained with the PersonaStack engineering workspace. This public README describes the project's status and scope.

## Security

The client must not put machine credentials in a web renderer, browser storage, URLs, process arguments, MCP payloads, or logs. The hosted app and API remain responsible for user, workspace, persona, and integration authorization. Local commands and files will be exposed only through PersonaStack's finite, authorized Desktop Control operations.

## Contributing

The first implementation milestone is to prove the required Cua, window-management, keyring, and session-lifecycle behavior on the supported Omarchy profile. Until then, this README describes intended work, not working functionality.
