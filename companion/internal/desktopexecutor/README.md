# Desktop executor

This package implements the producer-owned Cua operation families and the
installation-wide Desktop Control lease. It enforces the 90-second idle and
30-minute maximum lease bounds, config-version and persona-generation fences,
the finite operation-to-tool allowlist, command deadlines, bounded image results,
and command draining for release and revocation. Oversized PNG and JPEG
screenshots are converted to bounded JPEG results before Gateway frame encoding.

The filesystem and managed-process operation families are not implemented.
`NativeReady` stays false, and status reports the native executor unavailable.
This package is not connected to companion startup or the Gateway yet. Its
tests prove source-level dispatch policy only. They do not prove a real Cua call
or Omarchy desktop behavior.
