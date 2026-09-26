# cuaruntime

`cuaruntime` composes the integrity-checked Linux Cua installer with one
`cuamcp` child process. It reports diagnostic readiness only when the child
catalog starts, the driver's read-only permission check succeeds, and the
pinned driver's Linux health report passes. It does not prove the Omarchy
compositor, input plugin, unlocked-session state, or target-application parity.
Callers own the runtime lifetime and must keep Gateway readiness separate.
