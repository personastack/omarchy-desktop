# Local Arch package

`PKGBUILD` builds the x86_64 Electron app and Go companion from pinned source revisions. It is a local package recipe. It does not publish a repository or release artifact.

## Build and install

Use an Arch Linux build environment with `base-devel`, Git, Go, Node.js `24.21.0` or later, and SSH read access to the private `personastack-api` repository. The recipe downloads the npm `11.19.0` CLI from the npm registry and verifies its SHA-512 checksum. Keep the SSH agent and credentials outside the build files.

From this directory, run:

```sh
makepkg -si
```

The Go build uses the producer-owned API client at the revision pinned by `PKGBUILD`. The pinned npm CLI runs `npm ci` against the checked-in lockfile. The package includes the pinned Electron runtime, desktop entry, icon, companion, Electron/Chromium notices, Cua notices, and license files found for Go modules. It depends on `xorg-xwayland` because the app selects Electron's X11 backend for window controls.

The package metadata says `unknown` for the application license because this repository does not declare one. That value does not grant permission to redistribute the app. The package is intended for a local build by an authorized PersonaStack developer.

## Update

Review the new app and API source revisions. Update the two commit pins in `PKGBUILD`, increment `pkgrel` when only the recipe changes, then rebuild and install with `makepkg -si`.

## Roll back

The app has no automatic updater or package rollback hook. Keep a previously built package if you may need to revert. Restore it with:

```sh
sudo pacman -U /path/to/previously-built-personastack.pkg.tar.zst
```

The package owns files under `/usr`. It does not own or remove the user's Electron browser session, Linux Secret Service credential, or Codex and Claude configuration. The current app has no user-data migration to reverse. Package upgrade and rollback have not been acceptance-tested. Future user-data migrations need an explicit downgrade compatibility policy.

## Repair

To reinstall a package file built from the reviewed source, run:

```sh
sudo pacman -U /path/to/previously-built-personastack.pkg.tar.zst
```

This repairs package-owned files. The tray's **Repair Cua Service** action repairs only the pinned PersonaStack-managed Cua payload and leaves unrelated files untouched. It keeps remote control paused if repair fails.

## Uninstall

Remove the package with:

```sh
sudo pacman -Rns personastack
```

Pacman removes the package-owned runtime, launcher, desktop entry, and icon. The recipe has no removal hook for per-user Electron data, Secret Service credentials, or Codex/Claude configuration.

## Validation status

The current recipe pins app commit `488ad3e6a7acbf2194140abca0d45aa25b2eab56`, API client commit `fdf742c884168c4d6f2531b9cc65b966d018e7b3`, and package version `0.1.0-6`. On `eric-pc`, the Arch package build compiled the Electron app and companion. All 76 Node tests passed. The companion Go suite did not pass: Cua installer tests reported that the install path was not owned by PersonaStack, and related fixtures were missing under the `/build/tmp` host bind mount. `makepkg` therefore stopped before package creation. This run does not validate package installation or the complete Go suite.

The previous validated package `0.1.0-5` remains at `/tmp/personastack-0.1.0-5-x86_64.pkg.tar.zst` on `eric-pc`. It used app commit `3fec3310937ebb5b65852f5cd3632daba3486c8a`, passed all 67 Node tests and the full companion Go suite, and has SHA-256 `1f9f096fc0bf42f94a082282677e58bf8c29d2773619c7f2d011c2a136832467`. Its build used a local archive of the exact private API client revision, without forwarding SSH credentials. Package installation, keyring behavior, and Omarchy acceptance remain open. This recipe does not claim Omarchy support. Keep platform, installed-package, and full parity acceptance open.
