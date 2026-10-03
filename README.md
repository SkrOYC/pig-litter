# pig-litter

Optional subagent delegation for PiG.

The goal is to let a parent agent delegate focused tasks to child agents with separate conversations. The parent can continue working, inspect progress, send direction, stop work, and receive concise reports with clear outcomes.

Delegation belongs in an explicitly selected Resource or Piglet. PiG remains responsible for child Sessions, models, credentials, tool restrictions, permissions, and cleanup.

The intended experience includes reusable agent definitions, foreground and background work, and an interactive view for inspecting and directing children. Detailed child histories stay separate from the parent conversation unless requested.

## Develop with devenv

Install Nix and devenv 2.4 or newer with the [official devenv install guide](https://devenv.sh/getting-started/). The committed lock pins the Nix packages. `devenv.yaml` checks the installed CLI version.

Enter the pinned Go 1.27.1, Bun, and PiG 0.3.1 environment. Bun is available for this repository's tooling:

```sh
devenv shell
```

If you use direnv, review `.envrc` and run `direnv allow` once in this repository to activate the environment when you enter the directory.

Create a Go extension factory:

```sh
pig extension init ./extensions/my-extension --lang go
```

Edit the generated Go files, then build and load the extension without installing it:

```sh
pig install --validate-only --json ./extensions/my-extension
```

Run the extension while you develop it after configuring a provider:

```sh
pig -e ./extensions/my-extension
```

Save a Piglet definition at `piglet.yaml` and use paths relative to that file:

```yaml
name: my-agent
description: "My PiG agent"
extensions:
  - name: my-extension
    origins: [local:./extensions/my-extension]
```

Validate the Piglet without provider credentials:

```sh
pig piglet validate ./piglet.yaml
```

After configuring a provider, run the Piglet:

```sh
pig --piglet ./piglet.yaml
```

Run `devenv test` to create a temporary Go extension and check that PiG loads it, registers its generated `hello_ping` tool, and resolves a local Piglet extension origin. The check uses temporary PiG and workspace directories. It needs no model credential and leaves user settings untouched.

## License

Licensed under the [Apache License 2.0](LICENSE).
