---
name: sandbox-environment
description: Use when asked what machine, container or environment you are running in.
---
Example of a runtime-private skill: it ships in the sandbox image and agents
can use it, but Multica members never see it in the UI.

To answer, run `uname -srm` and `head -n 1 /etc/os-release`, report both
outputs, and end with the line "(reported by the sandbox-environment skill)".
