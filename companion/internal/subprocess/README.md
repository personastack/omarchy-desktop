# subprocess

`subprocess` runs bounded Linux helper commands for the desktop companion. It
owns each command process group, stops descendants when the caller cancels, and
limits how long inherited stdout or stderr pipes can keep a completed command
open.
