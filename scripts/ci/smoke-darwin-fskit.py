#!/usr/bin/env python3
"""Exercise a real FSKit mount against disposable localfs data only.

Usage: python3 scripts/ci/smoke-darwin-fskit.py /absolute/path/to/qrypt
Requires macOS 26+ and an installed, enabled FUSE-T FSKit extension.
"""

import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time


def wait_for(check, description, timeout=20):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.1)
    raise RuntimeError(f"timed out waiting for {description}")


def mount_entry(mount_point):
    table = subprocess.check_output(["/sbin/mount"], text=True, timeout=5)
    marker = f" on {mount_point} ("
    return next((line for line in table.splitlines() if marker in line), "")


def stop_core(binary, config, env, log=None):
    stdout = log if log is not None else subprocess.DEVNULL
    stderr = subprocess.STDOUT if log is not None else subprocess.DEVNULL
    subprocess.run(
        [str(binary), "core", "stop", "--config", str(config)],
        env=env, check=False, timeout=20,
        stdout=stdout, stderr=stderr,
    )


def check_io(root, verify_only=False):
    mounted = root / "mount with spaces" / "local"
    remote = root / "remote"

    def persisted(name, payload):
        target = remote / name
        wait_for(lambda: target.is_file() and target.read_bytes() == payload,
                 f"{name} to reach the localfs backend")
        if (mounted / name).read_bytes() != payload:
            raise RuntimeError(f"mounted contents differ for {name}")

    if verify_only:
        for name in ("closed.txt", "synced.txt", "saved.txt", "streamed.txt",
                     "copied.txt", "empty.txt", "truncated.txt"):
            if (mounted / name).read_bytes() != (remote / name).read_bytes():
                raise RuntimeError(f"contents changed after remount: {name}")
        print("remount: contents verified", flush=True)
        return

    seed = (remote / "seed.txt").read_bytes()
    if (mounted / "seed.txt").read_bytes() != seed:
        raise RuntimeError("seed read failed")
    if os.statvfs(mounted).f_bavail <= 0:
        raise RuntimeError("FSKit reports no free space")

    # No fsync: ordinary close must still publish staged writes.
    payload = os.urandom(2 * 1024 * 1024)
    (mounted / "closed.txt").write_bytes(payload)
    persisted("closed.txt", payload)
    print("write and close: persisted", flush=True)

    with (mounted / "synced.txt").open("wb") as stream:
        stream.write(seed)
        stream.flush()
        os.fsync(stream.fileno())
    persisted("synced.txt", seed)
    print("fsync: persisted", flush=True)

    (mounted / "save.tmp").write_bytes(payload)
    os.replace(mounted / "save.tmp", mounted / "saved.txt")
    persisted("saved.txt", payload)
    print("atomic rename: persisted", flush=True)

    with (mounted / "streamed.txt").open("wb", buffering=0) as stream:
        stream.write(b"first")
        time.sleep(0.4)  # Let the first upload generation be claimed.
        stream.write(b" second")
        stream.seek(0)
        stream.write(b"FIRST")
    persisted("streamed.txt", b"FIRST second")
    print("stream append and overwrite: persisted", flush=True)

    subprocess.run(["/bin/cp", str(mounted / "seed.txt"),
                    str(mounted / "copied.txt")], check=True, timeout=15)
    persisted("copied.txt", seed)
    (mounted / "empty.txt").touch()
    with (mounted / "closed.txt").open("r+b") as stream:
        stream.truncate(0)
    os.rename(mounted / "closed.txt", mounted / "truncated.txt")
    persisted("empty.txt", b"")
    persisted("truncated.txt", b"")
    # Keep a nonempty closed.txt for the remount check.
    (mounted / "closed.txt").write_bytes(seed)
    persisted("closed.txt", seed)
    print("copy, empty create and truncate: persisted", flush=True)


def smoke(binary):
    # Darwin's Unix socket paths are limited to 104 bytes. The per-user
    # /var/folders temporary root is too long for QRYPT_HOME/run/control.sock.
    root = Path(tempfile.mkdtemp(prefix="qrypt-fskit-smoke-", dir="/tmp")).resolve()
    mount_point = root / "mount with spaces"
    mount_point.mkdir()
    (root / "remote").mkdir()
    (root / "remote" / "seed.txt").write_bytes(b"qrypt FSKit smoke\n")
    config = root / "qrypt.toml"
    config.write_text(f'''version = "1"
[mount]
mount_point = "{mount_point}"
volume_name = "QryptFSKitSmoke"
options = {{ darwin = ["backend=fskit", "defer_permissions", "auto_xattr"] }}
[time]
ntp_enabled = false
[upload]
upload_delay = "150ms"
delete_delay = "50ms"
[[mounts]]
name = "local"
type = "localfs"
[mounts.params]
root_path = "{root}/remote"
''')
    env = {**os.environ, "QRYPT_HOME": str(root / "home"), "QRYPT_LANG": "en"}
    proc = None
    log_path = root / "mount.log"
    try:
        with log_path.open("ab") as log:
            for verify_only in (False, True):
                proc = subprocess.Popen(
                    [str(binary), "mount", "--config", str(config), "--stop-core"],
                    stdout=log, stderr=subprocess.STDOUT, env=env,
                    start_new_session=True,
                )
                wait_for(lambda: bool(mount_entry(mount_point)) or proc.poll() is not None,
                         "FSKit mount")
                entry = mount_entry(mount_point)
                if "(fuse-t," not in entry or ", fskit," not in entry:
                    raise RuntimeError(f"expected FUSE-T FSKit mount, got: {entry!r}")
                print(entry, flush=True)
                subprocess.run(
                    [sys.executable, __file__, "--verify" if verify_only else "--io", str(root)],
                    check=True, timeout=90,
                )
                proc.send_signal(signal.SIGTERM)
                proc.wait(timeout=30)
                if proc.returncode != 0:
                    raise RuntimeError(f"mount process exited {proc.returncode}")
                wait_for(lambda: not mount_entry(mount_point), "unmount")
                stop_core(binary, config, env, log=log)
        print("FSKit localfs mount smoke passed", flush=True)
    except BaseException:
        print(log_path.read_text(errors="replace"), file=sys.stderr)
        raise
    finally:
        if proc is not None and proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        stop_core(binary, config, env)
        if mount_entry(mount_point):
            subprocess.run(["/usr/sbin/diskutil", "unmount", "force", str(mount_point)],
                           check=False, timeout=20)
        if not mount_entry(mount_point):
            shutil.rmtree(root)
        else:
            print(f"mount still present; kept test directory: {root}", file=sys.stderr)


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] in ("--io", "--verify"):
        check_io(Path(sys.argv[2]), verify_only=sys.argv[1] == "--verify")
    elif sys.platform == "darwin" and len(sys.argv) == 2:
        smoke(Path(sys.argv[1]).expanduser().resolve(strict=True))
    else:
        raise SystemExit(__doc__)
