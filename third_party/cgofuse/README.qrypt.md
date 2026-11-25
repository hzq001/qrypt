# qrypt cgofuse patch

This directory contains `github.com/winfsp/cgofuse` v1.6.0 under its MIT
license. qrypt carries two small extensions:

- `FileSystemHost.SetCapNodeRWLock` allows a thread-safe filesystem to declare
  macFUSE's `FUSE_CAP_NODE_RWLOCK` capability.
- `FileSystemHost.Notify` resolves the optional Unix `fuse_invalidate_path`
  symbol so asynchronous qrypt mutations can invalidate kernel path caches.

Keep changes relative to v1.6.0 limited to these APIs and their wiring so the
fork can be removed when upstream provides equivalent options.

## macOS library preference (qrypt-specific)

`fuse/host_cgo.go` (the `__APPLE__` branch of `cgofuse_init_fuse`) tries
`/usr/local/lib/libfuse-t.dylib` before macFUSE.

Reason, observed with macFUSE 5.4.0 on macOS 27: without an approved macFUSE
kernel extension (System Settings > Privacy & Security), `mount_macfuse` mounts
through the FSKit backend. That backend never issues `flush`/`release` for a
write handle — the FUSE trace for `echo hi > file` is `Create fh=1`,
`Release fh=1`, `Write fh=2` with no `Flush` — and qrypt commits staged writes
only on `Flush`, so writes through such a mount stay in
`~/.qrypt/upload/<mount>/staging/` and never reach the backend. FUSE-T provides
the same operations without a kernel extension and does commit staged writes.

Prefer macFUSE again once its kernel extension is loaded: the VFS backend does
deliver `flush`/`release`.

Explicit `backend=fskit` mounts now use qrypt's mutation-time staging commit
contract, so they no longer depend on a close-time `Flush` callback. The FUSE-T
library preference remains unchanged. See
[FSKit diagnostics](../../docs/for-developer/debug.md#fuse-t-fskit-mounts) and the
disposable `scripts/ci/smoke-darwin-fskit.py` real-mount regression.
