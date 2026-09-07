# third_party

Code ossein did not write and does not own, carried in-tree rather than as a
module dependency. Each entry states why it cannot simply be imported, and
what would have to be true to drop it.

| directory | upstream | why it is here |
|---|---|---|
| `vz` | [Code-Hex/vz](https://github.com/Code-Hex/vz) | vmnet networking (upstream PR #205) is unreleased and cannot be added from outside the package — see [vz/FORK.md](vz/FORK.md) |
