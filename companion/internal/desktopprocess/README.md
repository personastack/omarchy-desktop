# Managed Linux process sessions

This Linux package runs commands in the selected user's login shell. It bounds command and stdin size, active and retained sessions, output ring size, read size, wait time, and execution timeout. It keeps stdout and stderr separate, returns output before exit, reports cursor gaps, supports stdin data/EOF, interrupt, cancellation, timeout, and kills remaining members of the managed process group after the leader exits.

Cleanup covers descendants that remain in the managed process group. A command that deliberately starts a new session can escape that group. The package does not claim cleanup for those detached descendants.

The caller must authorize every operation against the current installation, configuration, persona, session, lease, and revocation generation. This package has no scope checks of its own. The Linux `desktoplocal` adapter dispatches process calls through the executor's active lease and closes managed processes when the lease ends. Companion startup constructs the adapter, and the lifecycle controller dispatches Gateway commands through the executor. Linux process behavior still needs validation on Omarchy.
