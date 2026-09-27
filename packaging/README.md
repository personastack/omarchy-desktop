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
sudo pacman -U ./personastack-0.1.0-6-x86_64.pkg.tar.zst
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

On `eric-pc`, `makepkg --cleanbuild --noconfirm` built package `personastack 0.1.0-6` from app commit `488ad3e6a7acbf2194140abca0d45aa25b2eab56` and API client commit `fdf742c884168c4d6f2531b9cc65b966d018e7b3`. Node `26.10.0`, the pinned npm CLI `11.19.0`, and Go `1.27.1` compiled the app and companion. All 76 Node tests and the full companion Go test suite passed. `pacman -Qp --info` confirms `xorg-xwayland`. The package SHA-256 is `2c7aa3d9da93943fc64058fa5033bf5b7766e1150b22df33515bf51aaabe923d`. The artifact is at `/tmp/personastack-0.1.0-6-x86_64.pkg.tar.zst` on `eric-pc`.

An earlier attempt set `TMPDIR` to the bind-mounted build directory. Cua installer ownership tests failed there because the container's user-namespace ownership differed on the mount. The successful run kept Go temporary files in the container filesystem. The build used a local archive of the exact private API client revision, without forwarding SSH credentials. Package installation, keyring behavior, and Omarchy acceptance remain open. This recipe does not claim Omarchy support. Keep platform, installed-package, and full parity acceptance open.
