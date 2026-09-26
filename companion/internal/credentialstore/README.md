# Desktop credential store

This package keeps one API-issued Desktop Control installation record per exact PersonaStack app origin in the Linux user's Secret Service collection. It invokes `/usr/bin/secret-tool` with bounded input/output and never writes a plaintext fallback.

Enrollment, runtime lifecycle, keyring-provider detection, unlock behavior, and real Omarchy acceptance are outside this package. Unit tests use a fake `secret-tool`; they do not establish native Secret Service behavior.
