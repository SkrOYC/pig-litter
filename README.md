# pig-litter

Optional subagent delegation for PiG.

The goal is to let a parent agent delegate focused tasks to child agents with separate conversations. The parent can continue working, inspect progress, send direction, stop work, and receive concise reports with clear outcomes.

Delegation belongs in an explicitly selected Resource or Piglet. PiG remains responsible for child Sessions, models, credentials, tool restrictions, permissions, and cleanup.

The intended experience includes reusable agent definitions, foreground and background work, and an interactive view for inspecting and directing children. Detailed child histories stay separate from the parent conversation unless requested.

## License

Licensed under the [Apache License 2.0](LICENSE).
