# Local Arch package

`PKGBUILD` builds the x86_64 Electron app and Go companion from pinned source revisions. It is a local package recipe. It does not publish a repository or release artifact.

## Build and install

Use an Arch Linux build environment with `base-devel`, Git, Go, Node.js `24.21.0` or later, and SSH read access to the private `personastack-api` repository. The recipe downloads the npm `11.19.0` CLI from the npm registry and verifies its SHA-512 checksum. Keep the SSH agent and credentials outside the build files.

From this directory, run:

```sh
makepkg -si
```

The Go build uses the producer-owned API client at the revision pinned by `PKGBUILD`. The pinned npm CLI runs `npm ci` against the checked-in lockfile. The package includes the pinned Electron runtime, desktop entry, icon, companion, Electron/Chromium notices, Cua notices, and license files found for Go modules.

The package metadata says `unknown` for the application license because this repository does not declare one. That value does not grant permission to redistribute the app. The package is intended for a local build by an authorized PersonaStack developer.

## Update

Review the new app and API source revisions. Update the two commit pins in `PKGBUILD`, increment `pkgrel` when only the recipe changes, then rebuild and install with `makepkg -si`.

## Repair

To reinstall a package file built from the reviewed source, run:

```sh
sudo pacman -U ./personastack-0.1.0-1-x86_64.pkg.tar.zst
```

This repairs package-owned files. The in-app Cua repair action is not implemented yet.

## Uninstall

Remove the package with:

```sh
sudo pacman -Rns personastack
```

Pacman removes the package-owned runtime, launcher, desktop entry, and icon. The recipe has no removal hook for per-user Electron data, Secret Service credentials, or Codex/Claude configuration.

## Validation status

`makepkg` built the package in a disposable Arch `base-devel` container on `eric-pc`. That run used the pinned public app commit and an exact API client source snapshot in place of the private SSH source fetch. It passed the 33 Node tests, the full Go suite, packaging, and Arch package metadata checks. Package installation and Omarchy testing remain open. This recipe does not claim Omarchy support. Keep the platform, installed-package, and full parity acceptance checks open.
