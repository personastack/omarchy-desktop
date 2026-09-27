# Support profile

This package collects a local, read-only Linux support report for native acceptance. It records distribution and session facts, the Hyprland and app-managed Cua versions, and whether the Secret Service D-Bus name responds. Before reading the Cua version, it verifies the full app-managed driver tree against the pinned release checksums. It does not query credential contents or retain raw display names, session identifiers, or keyring output.

The report is diagnostic evidence only. A responding Secret Service name does not prove an unlocked keyring, and reported versions do not prove functional compatibility. Acceptance still requires the exact Omarchy profile and the complete native behavior matrix.
