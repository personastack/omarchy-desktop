# Hyprland session lock state

This package reads the active Hyprland instance's lock state through the
read-only `hyprctl -j locked` command. It requires the inherited
`XDG_RUNTIME_DIR` and `HYPRLAND_INSTANCE_SIGNATURE` values from the graphical
session. It never reads logind's `LockedHint`, since the compositor owns the
lockscreen state.

The monitor probes immediately, then once per configured interval. A failed
probe reports `unknown`, which keeps the Desktop Control executor fenced. The
default command timeout and poll interval are bounded. Tests inject the command
runner and environment lookup; they do not establish behavior on Omarchy.

The application must start the monitor only in its native Linux lifecycle and
pass its state updates to `desktopexecutor.Executor.SetSessionLockState`.
Native Omarchy acceptance remains required to verify environment inheritance,
compositor IPC availability, lock transitions, and unlock behavior.
