# pig-litter

Pig Litter is a small Go extension for PiG. The project's goal is optional subagent delegation. This bootstrap does not launch child sessions; it adds only a diagnostic command and status when you select it.

## Develop with devenv

Install Nix and devenv 2.4 or newer with the [official devenv install guide](https://devenv.sh/getting-started/). The lockfile pins PiG 0.3.1, Go 1.27.1, Bun, and tmux.

Enter the development environment:

```sh
devenv shell
```

## Select the extension

Run the extension directly:

```sh
pig -e ./extensions/pig-litter
```

Or select it through the local Piglet:

```sh
pig --piglet ./piglet.yaml
```

The [Piglet definition](piglet.yaml) resolves its extension path relative to `piglet.yaml`. Run plain `pig` to start PiG without selecting Pig Litter.

## Verify the bootstrap

Run the offline check to build and validate the Go extension and Piglet. It confirms that PiG registers `/pig-litter` and exposes no tools to the model:

```sh
devenv shell -- check-pig-extension
```

Run the project-local [verify-pig-litter skill](.agents/skills/verify-pig-litter/SKILL.md) to drive direct selection, Piglet selection, and plain PiG in isolated tmux sessions. It saves terminal evidence under `.pstack/evidence/pig-litter/` and removes only its own sessions and temporary PiG state:

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

## License

Licensed under the [Apache License 2.0](LICENSE).
