#!/usr/bin/env python3
"""Preserve profile files overwritten by the pinned AIO v1.11.0 startup."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import stat
import sys
import tempfile
from pathlib import Path


DEFAULT_HOME = Path("/home/gem")
STATE_NAME = ".morde-aio-profile-state"
EPHEMERAL_NAMES = frozenset({"code-server-ipc.sock"})
# Codex recreates disposable command shims in this tree, including symlinks.
CODEX_EPHEMERAL_PATH = "tmp"
MANAGED_PATHS = (
    ("bashrc", ".bashrc", "file"),
    ("code-server-vscode", ".config/code-server/vscode", "directory"),
    ("jupyter", ".jupyter", "directory"),
    ("matplotlib", ".config/matplotlib/matplotlibrc", "file"),
    ("browser-preferences", ".config/browser/Default/Preferences", "file"),
    ("opencode", ".config/opencode/config.json", "file"),
    ("claude", ".claude/settings.json", "file"),
    # The fixed image recursively chowns the whole .codex directory whenever it
    # renders either config file, so preserve the tree rather than only those two.
    ("codex", ".codex", "directory"),
    ("agent-skills", ".agents/skills", "directory"),
    ("fcitx5", ".config/fcitx5", "directory"),
)


class SafeguardError(Exception):
    pass


def is_ephemeral(root: Path, path: Path) -> bool:
    return path.name in EPHEMERAL_NAMES or (root.name == ".codex" and path == root / CODEX_EPHEMERAL_PATH)


def fsync_dir(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_json(path: Path, value: dict) -> None:
    fd, tmp_name = tempfile.mkstemp(prefix=".state.", dir=path.parent)
    tmp = Path(tmp_name)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(value, stream, sort_keys=True, separators=(",", ":"))
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.chown(tmp, 0, 0)
        os.replace(tmp, path)
        fsync_dir(path.parent)
    finally:
        try:
            tmp.unlink()
        except FileNotFoundError:
            pass


def check_safe_parent(home: Path, relative: str) -> Path:
    """Resolve a fixed managed path while rejecting symlinked path components."""
    if home.is_symlink() or not home.is_dir():
        raise SafeguardError("profile home is not a real directory")
    target = home
    components = Path(relative).parts
    for component in components[:-1]:
        target = target / component
        try:
            info = target.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SafeguardError("managed config has an unsafe parent")
    return home.joinpath(*components)


def ensure_state_dir(home: Path) -> Path:
    state = home / STATE_NAME
    try:
        info = state.lstat()
    except FileNotFoundError:
        state.mkdir(mode=0o700)
        os.chown(state, 0, 0)
        info = state.lstat()
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SafeguardError("profile state path is not a real directory")
    if info.st_uid != 0 or stat.S_IMODE(info.st_mode) & 0o077:
        raise SafeguardError("profile state directory is not root-only")
    return state


def assert_supported_tree(path: Path, kind: str) -> bool:
    try:
        root_info = path.lstat()
    except FileNotFoundError:
        return False
    if kind == "file" and not stat.S_ISREG(root_info.st_mode):
        raise SafeguardError("managed config is not a regular file")
    if kind == "directory" and not stat.S_ISDIR(root_info.st_mode):
        raise SafeguardError("managed config is not a directory")

    if stat.S_ISDIR(root_info.st_mode):
        for current, dirs, files in os.walk(path, topdown=True, followlinks=False):
            base = Path(current)
            dirs[:] = [name for name in dirs if not is_ephemeral(path, base / name)]
            for name in dirs:
                item = base / name
                info = item.lstat()
                if not stat.S_ISDIR(info.st_mode):
                    raise SafeguardError("managed config tree contains a symlink or special file")
            for name in files:
                if is_ephemeral(path, base / name):
                    continue
                info = (base / name).lstat()
                if not stat.S_ISREG(info.st_mode):
                    raise SafeguardError("managed config tree contains a symlink or special file")
    return True


def copy_path(source: Path, destination: Path, root: Path | None = None) -> None:
    if root is None:
        root = source
    info = source.lstat()
    if stat.S_ISDIR(info.st_mode):
        destination.mkdir(mode=stat.S_IMODE(info.st_mode))
        for child in sorted(source.iterdir(), key=lambda item: item.name):
            if is_ephemeral(root, child):
                continue
            copy_path(child, destination / child.name, root)
        shutil.copystat(source, destination, follow_symlinks=False)
        os.chown(destination, info.st_uid, info.st_gid, follow_symlinks=False)
        fsync_dir(destination)
    elif stat.S_ISREG(info.st_mode):
        shutil.copyfile(source, destination, follow_symlinks=False)
        shutil.copystat(source, destination, follow_symlinks=False)
        os.chown(destination, info.st_uid, info.st_gid, follow_symlinks=False)
        with destination.open("rb") as stream:
            os.fsync(stream.fileno())
    else:
        raise SafeguardError("unsupported object in managed config")


def fingerprint(path: Path, kind: str) -> str | None:
    if not assert_supported_tree(path, kind):
        return None
    digest = hashlib.sha256()

    def visit(item: Path, relative: str) -> None:
        info = item.lstat()
        mode = stat.S_IMODE(info.st_mode)
        digest.update(relative.encode("utf-8", "surrogateescape") + b"\0")
        digest.update(f"{stat.S_IFMT(info.st_mode)}:{mode}:{info.st_uid}:{info.st_gid}\0".encode())
        if stat.S_ISDIR(info.st_mode):
            for child in sorted(item.iterdir(), key=lambda value: value.name):
                if is_ephemeral(path, child):
                    continue
                child_relative = f"{relative}/{child.name}" if relative else child.name
                visit(child, child_relative)
        else:
            with item.open("rb") as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(chunk)

    visit(path, "")
    return digest.hexdigest()


def has_nonempty_data(path: Path, kind: str) -> bool:
    if not assert_supported_tree(path, kind):
        return False
    if kind == "file":
        return path.stat().st_size > 0
    for current, dirs, files in os.walk(path):
        base = Path(current)
        dirs[:] = [name for name in dirs if not is_ephemeral(path, base / name)]
        for name in files:
            if is_ephemeral(path, base / name):
                continue
            if (Path(current) / name).stat().st_size > 0:
                return True
    return False


def save_pending(state: Path, name: str, target: Path, relative: str, kind: str) -> None:
    pending = state / f"{name}.pending"
    if pending.exists():
        return
    temp = Path(tempfile.mkdtemp(prefix=f".{name}.pending.", dir=state))
    try:
        exists = target.exists()
        if exists:
            copy_path(target, temp / "content")
        manifest = {"path": relative, "kind": kind, "exists": exists}
        manifest_path = temp / "manifest.json"
        manifest_path.write_text(json.dumps(manifest, sort_keys=True) + "\n", encoding="utf-8")
        os.chmod(manifest_path, 0o600)
        os.chown(manifest_path, 0, 0)
        with manifest_path.open("rb") as stream:
            os.fsync(stream.fileno())
        fsync_dir(temp)
        os.replace(temp, pending)
        fsync_dir(state)
    finally:
        if temp.exists():
            shutil.rmtree(temp)


def prepare(home: Path) -> None:
    state = ensure_state_dir(home)
    for name, relative, kind in MANAGED_PATHS:
        target = check_safe_parent(home, relative)
        current = fingerprint(target, kind)
        pending = state / f"{name}.pending"
        if pending.exists():
            continue
        baseline_path = state / f"{name}.json"
        if baseline_path.exists():
            try:
                baseline_data = json.loads(baseline_path.read_text(encoding="utf-8"))
                baseline = baseline_data.get("baseline")
            except (OSError, ValueError, AttributeError) as exc:
                raise SafeguardError("profile state is unreadable") from exc
            if current != baseline:
                save_pending(state, name, target, relative, kind)
        elif current is not None and has_nonempty_data(target, kind):
            # On first boot, only pre-existing non-empty user data is protected.
            # Empty files/trees are left for AIO to initialize normally.
            save_pending(state, name, target, relative, kind)


def remove_target(target: Path) -> None:
    try:
        info = target.lstat()
    except FileNotFoundError:
        return
    if stat.S_ISDIR(info.st_mode):
        shutil.rmtree(target)
    else:
        target.unlink()


def restore_one(state: Path, home: Path, name: str, relative: str, kind: str) -> None:
    target = check_safe_parent(home, relative)
    current = fingerprint(target, kind)
    atomic_json(state / f"{name}.json", {"baseline": current})

    pending = state / f"{name}.pending"
    if not pending.exists():
        return
    if pending.is_symlink() or not pending.is_dir():
        raise SafeguardError("pending profile backup is unsafe")
    try:
        manifest = json.loads((pending / "manifest.json").read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise SafeguardError("pending profile backup is unreadable") from exc
    if (
        set(manifest) != {"path", "kind", "exists"}
        or manifest.get("path") != relative
        or manifest.get("kind") != kind
        or not isinstance(manifest.get("exists"), bool)
    ):
        raise SafeguardError("pending profile backup does not match its target")

    if manifest["exists"]:
        backup = pending / "content"
        if not assert_supported_tree(backup, kind):
            raise SafeguardError("pending profile backup content is missing")
        target.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
        staging = Path(tempfile.mkdtemp(prefix=f".morde-restore-{name}.", dir=target.parent))
        staged_path = staging / "content"
        try:
            copy_path(backup, staged_path, target)
            remove_target(target)
            os.replace(staged_path, target)
            fsync_dir(target.parent)
        finally:
            shutil.rmtree(staging, ignore_errors=True)
    else:
        remove_target(target)
    shutil.rmtree(pending)
    fsync_dir(state)


def restore(home: Path) -> None:
    state = ensure_state_dir(home)
    for name, relative, kind in MANAGED_PATHS:
        restore_one(state, home, name, relative, kind)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("prepare", "restore"))
    parser.add_argument("--home", type=Path, default=DEFAULT_HOME, help=argparse.SUPPRESS)
    args = parser.parse_args()
    home = args.home
    try:
        if os.geteuid() != 0 and home == DEFAULT_HOME:
            raise SafeguardError("profile safeguard must run as root")
        if args.command == "prepare":
            prepare(home)
        else:
            restore(home)
        return 0
    except Exception as exc:  # Never echo config values or file contents.
        print(f"Morde profile safeguard failed: {type(exc).__name__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
