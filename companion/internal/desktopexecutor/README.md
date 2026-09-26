# Desktop executor

This package implements the producer-owned Cua operation families and the
installation-wide Desktop Control lease. It enforces the 90-second idle and
30-minute maximum lease bounds, config-version and persona-generation fences,
the finite operation-to-tool allowlist, command deadlines, bounded image results,
and command draining for release and revocation. Oversized PNG and JPEG
screenshots are converted to bounded JPEG results before Gateway frame encoding.

When constructed with `NewWithLocalOperations`, the executor dispatches the
producer-owned file and managed-process operation families through the same
configuration, persona-generation, and lease checks as Cua calls. Resource
handles are closed on lease release and scoped revocation. Unconfirmed cleanup
leaves the executor unavailable. Session lock state starts unknown. Control stays
fenced until an unlock is observed. Locking cancels in-flight work, revokes the
active lease, closes local handles and processes, and fences late results.
`Close` stops lease monitoring, cancels active commands, closes resources, and
can retry unconfirmed shutdown cleanup. The executor expires idle leases every
five seconds, renews the idle window while a managed process runs, and caps
process timeouts at the lease hard deadline. The companion does not construct
this executor or connect it to Gateway startup yet. Tests prove the dispatch and
lock-fence contracts with fakes. They do not prove real Cua calls or Omarchy
desktop behavior.
