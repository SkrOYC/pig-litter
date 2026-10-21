# Terminal evidence and owned runs

The maintained terminal verifier now proves the capture interval and isolates each wrapper invocation's writable resources. This implements the scoped changes for [issue 25](https://github.com/SkrOYC/pig-litter/issues/25) and [issue 26](https://github.com/SkrOYC/pig-litter/issues/26).

## Capture the complete interval

A retained pane supervisor starts PiG only after the recorder acknowledges readiness. After PiG exits, the supervisor writes a private process-completion record and stays alive while the owner closes the recording pipe. The owner drains and joins the recorder before releasing the supervisor.

This sequence accounts for tmux 3.7c's `remain-on-exit` behavior. A program can exit while its recording pipe remains open. Conversely, detaching a pipe can produce EOF while the program continues. Neither event alone proves complete capture.

The evidence combines owned process identities, the private completion record, an output boundary, explicit transport closure, and recorder completion. Saved bytes are bounded across restarts. Excess bytes are drained and still checked for hostile payloads. Missing, detached, replaced, interrupted, empty, or truncated capture remains inconclusive. Raw output, injected input, rendered frames, and terminal state remain separate artifacts.

## Own resources before building

`scripts/verify-terminal-ui.sh` creates a short private root before its first Go build. The root contains binaries, build and module caches, temporary directories, and runtime state. The wrapper records allocation identities and validates the verifier's cleanup acknowledgement before removing its own root. Evidence remains outside every removable resource.

Private Go builds use the supported `-modcacherw` option. The first overlap run preserved its failed cleanup result when read-only extracted module directories prevented removal. The corrected run kept the same assertions and cleaned each allocation independently.

## Limit live input claims

The generic negotiation-aware encoder proposed in [issue 24](https://github.com/SkrOYC/pig-litter/issues/24) is not being introduced. Tmux's observed extended-key mode does not expose the Kitty flag bits required to certify that negotiation.

The live driver uses ordinary text and tmux-owned named keys. It rejects control sequences and invalid UTF-8 before delivery. Exact CSI-u bytes remain covered by the released public decoder fixtures. Those fixtures do not claim that live Kitty negotiation occurred.

## Verification

Run the focused verifier suite in the declared environment.

```sh
devenv shell -- go -C scripts test -race ./cmd/verify-tui
```

The final race run passed 64 top-level tests. Real PTY fixtures exercise erased title and clear-screen sequences, capture loss, overflow, substituted evidence, missing private records, and hostile bytes beyond the saved limit.

Two overlapping maintained wrapper runs passed. Run A used direct `receipt-control`; run B used direct `all`. A removed its allocation while B retained its tmux server. B then completed another captured generation and removed its own allocation. Both evidence sets survived. Separate local Piglet `all` and unselected `all` drives also passed.

Bulk evidence is retained in the isolated reflection-terminal checkout under `.pstack/reflection-terminal/`. `verified-evidence.json` records result and allocation digests, raw hashes, process exits, removed roots, and retained evidence. `source-digests.json` identifies the tested source. The independent PR verdict records the frozen commit and patch identity.

The product and six model-tool contracts remain unchanged. These checks target the declared PiG and SDK v0.4.1 pins. A separate v0.5.1 diagnostic found SDK compilation and standalone registration incompatibilities; this change does not claim support for that newer host.
