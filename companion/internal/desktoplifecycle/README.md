# Desktop Control lifecycle

This package owns the Linux companion's managed Cua runtime, scoped native
executor, Hyprland lock monitor, API enrollment/attachment, and outbound
Gateway connection. It keeps credential and gateway state in native process
memory and exposes only installation ID and finite readiness through the
bridge.

`Prepare` is called only from the registered PersonaStack setup bridge. It
requires an observed unlocked Hyprland session before installing or starting
Cua. The executor begins locked-unknown and cannot accept gateway commands
until agent-gateway completes its API-owned session claim and sends the ready
frame. Prepare waits for a fresh heartbeat acknowledgement before reporting
Gateway readiness to hosted setup. Lock read failures fence the executor.
Gateway loss drops its command lease before reconnect. Startup recovery resumes
an active installation after the login session unlocks. A confirmed empty
aggregate stops the Gateway, Cua child, and lock monitor. Setup can remain
unconfigured for up to ten minutes while its first workspace config is saved.

Go fakes establish local orchestration and wire ordering only. They do not
prove the behavior of Cua, Secret Service, Gateway authorization, Hyprland, or
Omarchy. Keep those acceptance checks open until run on the supported profile.
