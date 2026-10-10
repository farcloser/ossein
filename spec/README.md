# ossein formal spec

`lifecycle.qnt` models the intent of the backgrounded-instance lifecycle
(`buildkit --detach`, `stop`, `gc`, `gc --prune-cache`), the image cache
(`pull`, `run --pull=<policy>`), and the one-shot commands, as a state machine
with invariants. It is written from the public surface only: the `--help`
output, `README.md`, and the acceptance criteria in `docs/TODO.md`. No source
was read. It is part of the spec-driven testing experiment described in the
farcloser-level `SPEC-DRIVEN-TESTING.md`.

The model is deliberately the strict reading of every sentence in the docs.
Where the docs are silent it does not guess: the choice is marked `Q<n>` in
the spec and listed below. Answers go into the spec, not into this list.

## Run it

```
quint typecheck spec/lifecycle.qnt
quint run spec/lifecycle.qnt --backend=typescript --invariant=all_invariants --max-samples=300
quint run spec/lifecycle.qnt --backend=typescript --mbt --invariant=all_invariants \
  --out-itf='spec/traces/{seq}.itf.json' --max-samples=200
```

`--backend=typescript` matters: the default backend downloads a Rust
evaluator at run time, which our toolchain does not allow.

## Action to command mapping (for the driver)

| Model action | Command | Oracle |
|---|---|---|
| `buildkitDetach` | `ossein buildkit --detach [--cache <key>]` | exit code; `instances` (Q1) |
| `stopOne` | `ossein stop <id>` | exit code; `instances` |
| `stopAll` | `ossein stop` | `instances` empty |
| `crash` | `kill -9` the owning process | none (environment) |
| `gc` | `ossein gc` | no dead entries remain |
| `gcPrune` | `ossein gc --prune-cache` | `caches` |
| `pull` | `ossein pull <image>` | exit code; `images` |
| `runOnce` | `ossein run --pull=<policy> <image> true` | exit code; `images`, `instances` unchanged |
| `probe` | `ossein doctor`, `ossein buildkit --print-cache`, `ossein version` | nothing changed |

## Open questions

1. **Observability of instances.** There is no `list` command (TODO.md
   plans one at M4). The model's `instances` map has no public projection
   today, so a driver cannot compare it. Either `stop` with no ids must
   report what it stopped, or a `list` command is needed. This blocks
   replay of every instance invariant.
2. **Dead lock holder.** README says a cache is "locked while in use" and a
   second `buildkit` "fails fast". After `kill -9`, is the lock released
   (flock semantics) or does a state-file pre-check still refuse? The model
   says a dead holder never blocks (`refusalMeansLiveHolder`). If the
   pre-check reads state files, this invariant will fail on a real run, and
   that would be a bug worth the whole experiment.
3. **`stop <unknown-id>`.** Error, or silent success? The model only allows
   known ids.
4. **`stop <dead-id>`.** Does stopping a crashed instance reap its state
   entry, or is that only `gc`'s job? The model lets `stop` reap.
5. **Failed pull.** Is a registry failure surfaced as a non-zero exit with
   the image cache untouched? The model assumes yes and never a partial
   entry.
6. **`--pull=missing` when the image is local.** The help says it "skips the
   registry". Observable only with the network cut. The model does not
   distinguish the two paths; a fault-injection phase would.
7. **Atomicity of failures.** The model asserts that any failed command
   leaves instances unchanged. A `buildkit --detach` that boots the VM and
   then fails must not leave a state entry behind. Is that the intent?
8. **Foreground `buildkit`.** Without `--detach` the command blocks. Does
   Ctrl-C behave as `stop` with the default grace? Not modelled.
9. **Cache creation on failure.** If `buildkit --detach` fails after
   creating a new cache volume, does the volume stay? The model creates the
   volume only on success.
