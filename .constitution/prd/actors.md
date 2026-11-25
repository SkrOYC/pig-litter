# Actors

## PiG operator

The operator runs a PiG workflow, configures its limits and agent profiles, and can open a child overlay to follow or steer any dependent child in the root's tree. In a headless run, the operator uses the workflow's results without a terminal view. Ending the root halts its tree, and reopening the root brings the tree back. To stop work for good, the operator tells the root to stop its children.

## Root session

The root heads a tree. The operator starts it, or another root launches it as a detached session. It is the only session of its tree that other local sessions can discover and message. It can launch dependent children and detached sessions.

## Parent session

The parent launches a dependent child with a brief and per-launch settings. It coordinates with that child by message and can wait for it when it needs a result. It can check the child's state, pause it, or stop it. Every root and every dependent child with children is a parent.

## Child session

The child executes a delegated assignment in a fresh conversation with the permissions granted to it. It exchanges messages only with its parent and its own children, and it can delegate further unless it is at the depth limit or read-only. Its parent receives its handbacks. The child remains available for follow-up work after it finishes a turn.

## Detached session

A detached session executes a larger assignment as an independent root with its own configuration. It reports to its launcher by peer message and stays available until its launcher stops it or the operator ends it.

## Independent peer

An independently started PiG root or Claude Code session can discover eligible local roots and exchange messages with them. A Claude Code peer may serve as a workflow lead. Its launcher owns its lifecycle.
