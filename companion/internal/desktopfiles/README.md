# Desktop filesystem operations

This package provides bounded filesystem operations for the Linux companion: metadata, directory listing, open/read/read-lines, close, create/replace/append, exact-match patch, directory creation, move, remove, and content search.

Linux x86_64 replacement writes preserve ownership, mode bits, extended attributes, access and modification timestamps, and supported inode flags. Replacements fail before touching the destination when metadata exceeds the copy limit or includes inode flags that this package cannot carry safely. Linux architectures other than x86_64 currently fail closed for replacement metadata and no-replace moves.

Every caller must bind paths and operations to the authenticated installation, configuration, persona, session, and active desktop-control lease. This package has no caller authorization of its own. The Linux `desktoplocal` adapter maps these operations into the executor's active lease. Companion startup constructs the adapter, and the lifecycle controller dispatches Gateway commands through the executor. Process execution is provided by a separate package. Omarchy filesystem behavior remains unverified.
