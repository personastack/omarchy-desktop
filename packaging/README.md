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
sudo pacman -U ./personastack-0.1.0-5-x86_64.pkg.tar.zst
```

This repairs package-owned files. The tray's **Repair Cua Service** action repairs only the pinned PersonaStack-managed Cua payload and leaves unrelated files untouched. It keeps remote control paused if repair fails.

## Uninstall

Remove the package with:

```sh
sudo pacman -Rns personastack
```

Pacman removes the package-owned runtime, launcher, desktop entry, and icon. The recipe has no removal hook for per-user Electron data, Secret Service credentials, or Codex/Claude configuration.

## Validation status

`makepkg` built package `personastack 0.1.0-5` in a disposable Arch `base-devel` container on `eric-pc`. The recipe pins app commit `3fec3310937ebb5b65852f5cd3632daba3486c8a` and API client commit `fdf742c884168c4d6f2531b9cc65b966d018e7b3`. The run used Node `26.10.0`, npm `11.19.0`, and Go `1.27.1`. All 67 Node tests and the full companion Go test suite passed in the package build. `pacman -Qp --info` confirms `xorg-xwayland`; package readback confirms it contains the Google Services OAuth navigation code. The package SHA-256 is `1f9f096fc0bf42f94a082282677e58bf8c29d2773619c7f2d011c2a136832467`. The artifact is retained on `eric-pc` at `/tmp/personastack-0.1.0-5-x86_64.pkg.tar.zst`. The temporary build used a local archive of the exact API client commit instead of fetching the private SSH source, so no credentials were forwarded. Package installation and Omarchy testing remain open. This recipe does not claim Omarchy support. Keep platform, installed-package, and full parity acceptance open.
