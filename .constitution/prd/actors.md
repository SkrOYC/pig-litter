# Actors

## PiG operator

The operator runs a PiG workflow, configures its limits and agent profiles, and can open a child overlay to follow or steer work. In a headless run, the operator uses the workflow's results without a terminal view. Ending the root ends the managed session tree.

## Parent session

The parent launches a child with a brief and per-launch settings. It can delegate further through eligible children, coordinate by message, and wait for a direct child when it needs a result. It can check whether its own child is active, pause it, or stop it.

## Child session

The child executes a delegated assignment in a fresh conversation with the permissions granted to it. It can send updates, receive steers, and delegate if its permissions allow. Its parent receives eligible final responses. The child remains available for follow-up work after it finishes a turn.

## Independent peer

An independently started PiG or Claude Code session can discover eligible local peers and exchange messages. A Claude Code peer may serve as a workflow lead. Its launcher owns its lifecycle.
