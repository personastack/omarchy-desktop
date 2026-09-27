# cuainstaller

`cuainstaller` installs Cua Driver 0.30.1, the Linux x86_64 candidate in `support-profile.json`. It accepts only the published archive with its fixed SHA-256, validates every archived file, and writes a private versioned runtime under the user's data directory. It preserves the bundled Cua MIT license and Node runtime MPL-2.0 notices. It does not run Cua, install the optional Hyprland plugin, or claim Omarchy compatibility. The caller must keep lifecycle and runtime readiness separate from installation success.
