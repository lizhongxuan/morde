#!/usr/bin/env python3
"""Preserve profile files overwritten by the pinned AIO v1.11.0 startup."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
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
AIO_UID = 1000
CHROMIUM_BROWSER_METRIC = re.compile(r"BrowserMetrics-[A-F0-9]{8}-[A-F0-9]{3,4}\.pma\Z")
CHROMIUM_SINGLETON_NAMES = ("SingletonLock", "SingletonCookie", "SingletonSocket")
WORKSPACE_DIRECTORIES = (
    ("DESKTOP", "Desktop"),
    ("DOWNLOAD", "Downloads"),
    ("DOCUMENTS", "Documents"),
    ("MUSIC", "Music"),
    ("PICTURES", "Pictures"),
    ("VIDEOS", "Videos"),
)
MANAGED_PATHS = (
    ("bashrc", ".bashrc", "file"),
    ("code-server-vscode", ".config/code-server/vscode", "directory"),
    ("jupyter", ".jupyter", "directory"),
    ("matplotlib", ".config/matplotlib/matplotlibrc", "file"),
    ("user-directories", ".config/user-dirs.dirs", "file"),
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
        owner = path.parent.stat()
        os.chown(tmp, owner.st_uid, owner.st_gid)
        os.replace(tmp, path)
        fsync_dir(path.parent)
    finally:
        try:
            tmp.unlink()
        except FileNotFoundError:
            pass


def atomic_text(path: Path, value: str, home: Path) -> None:
    path.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
    parent_info = path.parent.lstat()
    if stat.S_ISLNK(parent_info.st_mode) or not stat.S_ISDIR(parent_info.st_mode):
        raise SafeguardError("workspace profile parent is unsafe")
    try:
        existing = path.lstat()
    except FileNotFoundError:
        existing = None
    if existing is not None and not stat.S_ISREG(existing.st_mode):
        raise SafeguardError("workspace profile file is not regular")
    if existing is not None:
        owner = (existing.st_uid, existing.st_gid)
    elif os.geteuid() == 0:
        owner = (AIO_UID, AIO_UID)
    else:
        owner = (home.stat().st_uid, home.stat().st_gid)
    mode = stat.S_IMODE(existing.st_mode) if existing is not None else 0o644
    fd, tmp_name = tempfile.mkstemp(prefix=".workspace-defaults.", dir=path.parent)
    tmp = Path(tmp_name)
    try:
        os.fchmod(fd, mode)
        os.fchown(fd, *owner)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(value)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(tmp, path)
        fsync_dir(path.parent)
    finally:
        try:
            tmp.unlink()
        except FileNotFoundError:
            pass


def directory_has_data(path: Path) -> bool:
    try:
        info = path.lstat()
    except FileNotFoundError:
        return False
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        return True
    try:
        return next(path.iterdir(), None) is not None
    except OSError as exc:
        raise SafeguardError("existing user directory cannot be inspected") from exc


def ensure_workspace_directory(workspace: Path, name: str) -> None:
    path = workspace / name
    try:
        info = path.lstat()
    except FileNotFoundError:
        path.mkdir(mode=0o770)
        if os.geteuid() == 0:
            os.chown(path, AIO_UID, AIO_UID)
        return
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SafeguardError("Workspace default directory is unsafe")


def workspace_defaults(home: Path) -> None:
    layout = os.environ.get("MORDE_WORKSPACE_LAYOUT")
    if layout == "legacy":
        return
    if layout != "workspace":
        raise SafeguardError("workspace layout is unavailable")
    workspace = check_safe_parent(home, "workspace")
    try:
        workspace_info = workspace.lstat()
    except OSError as exc:
        raise SafeguardError("Workspace mount is unavailable") from exc
    if stat.S_ISLNK(workspace_info.st_mode) or not stat.S_ISDIR(workspace_info.st_mode):
        raise SafeguardError("Workspace mount is unsafe")

    user_dirs = check_safe_parent(home, ".config/user-dirs.dirs")
    try:
        user_dirs_info = user_dirs.lstat()
    except FileNotFoundError:
        user_dirs_info = None
        lines: list[str] = []
    else:
        if not stat.S_ISREG(user_dirs_info.st_mode):
            raise SafeguardError("user directory configuration is unsafe")
        lines = user_dirs.read_text(encoding="utf-8").splitlines()
    updated: list[str] = []
    seen: set[str] = set()
    changed = False
    for line in lines:
        key = next((f"XDG_{kind}_DIR" for kind, _ in WORKSPACE_DIRECTORIES if line.startswith(f"XDG_{kind}_DIR=")), "")
        if not key:
            updated.append(line)
            continue
        if key in seen:
            raise SafeguardError("user directory configuration has duplicate entries")
        seen.add(key)
        name = next(name for kind, name in WORKSPACE_DIRECTORIES if key == f"XDG_{kind}_DIR")
        old_values = {f'"$HOME/{name}"', f'"${{HOME}}/{name}"', f'"$HOME/{name}/"'}
        if line.split("=", 1)[1].strip() in old_values and not directory_has_data(home / name):
            ensure_workspace_directory(workspace, name)
            line = f'{key}="$HOME/workspace/{name}"'
            changed = True
        updated.append(line)
    for kind, name in WORKSPACE_DIRECTORIES:
        key = f"XDG_{kind}_DIR"
        if key not in seen and not directory_has_data(home / name):
            ensure_workspace_directory(workspace, name)
            updated.append(f'{key}="$HOME/workspace/{name}"')
            changed = True
    if changed or user_dirs_info is None:
        atomic_text(user_dirs, "\n".join(updated) + "\n", home)

    preferences = check_safe_parent(home, ".config/browser/Default/Preferences")
    try:
        preferences_info = preferences.lstat()
    except FileNotFoundError:
        preferences_info = None
        value: dict = {}
    else:
        if not stat.S_ISREG(preferences_info.st_mode):
            raise SafeguardError("browser preferences are unsafe")
        try:
            value = json.loads(preferences.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise SafeguardError("browser preferences are unreadable") from exc
        if not isinstance(value, dict):
            raise SafeguardError("browser preferences are invalid")
    download = value.setdefault("download", {})
    if not isinstance(download, dict):
        raise SafeguardError("browser download preferences are invalid")
    current = download.get("default_directory", "")
    normalized = os.path.normpath(str(current).replace("${HOME}", str(home)).replace("$HOME", str(home)).replace("~", str(home), 1))
    default_downloads = {"Downloads", str(home / "Downloads"), str(home / "Downloads/")}
    if (not current or normalized in default_downloads) and not directory_has_data(home / "Downloads"):
        ensure_workspace_directory(workspace, "Downloads")
        download["default_directory"] = str(workspace / "Downloads")
        download["directory_upgrade"] = True
        atomic_text(preferences, json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n", home)


def secure_workspace_root(home: Path) -> None:
    layout = os.environ.get("MORDE_WORKSPACE_LAYOUT")
    if layout == "legacy":
        return
    if layout != "workspace":
        raise SafeguardError("Workspace layout is unavailable")

    workspace = home / "workspace"
    try:
        info = workspace.lstat()
    except OSError as exc:
        raise SafeguardError("Workspace mount is unavailable") from exc
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SafeguardError("Workspace mount is unsafe")
    if info.st_uid not in {os.geteuid(), AIO_UID} or info.st_mode & stat.S_IWOTH:
        raise SafeguardError("Workspace mount ownership is unsafe")

    try:
        group_id = int(os.environ.get("USER_GID", str(AIO_UID)))
    except ValueError as exc:
        raise SafeguardError("Workspace user group is invalid") from exc
    if group_id < 0:
        raise SafeguardError("Workspace user group is invalid")

    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        fd = os.open(workspace, flags)
    except OSError as exc:
        raise SafeguardError("Workspace mount is unsafe") from exc
    try:
        opened = os.fstat(fd)
        if not stat.S_ISDIR(opened.st_mode) or opened.st_dev != info.st_dev or opened.st_ino != info.st_ino:
            raise SafeguardError("Workspace mount changed during validation")
        os.fchown(fd, 0 if os.geteuid() == 0 else os.geteuid(), group_id)
        os.fchmod(fd, 0o3770)
        secured = os.fstat(fd)
        if (secured.st_uid, secured.st_gid, stat.S_IMODE(secured.st_mode)) != (
            0 if os.geteuid() == 0 else os.geteuid(), group_id, 0o3770
        ):
            raise SafeguardError("Workspace mount permissions could not be secured")
        current = workspace.lstat()
        if current.st_dev != secured.st_dev or current.st_ino != secured.st_ino:
            raise SafeguardError("Workspace mount changed during validation")
    finally:
        os.close(fd)


def prune_browser_metrics(home: Path) -> int:
    directory = check_safe_parent(home, ".config/browser/DeferredBrowserMetrics")
    try:
        info = directory.lstat()
    except FileNotFoundError:
        return 0
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SafeguardError("browser metrics directory is unsafe")
    removed = 0
    with os.scandir(directory) as entries:
        for entry in entries:
            if not CHROMIUM_BROWSER_METRIC.fullmatch(entry.name):
                continue
            try:
                file_info = entry.stat(follow_symlinks=False)
            except FileNotFoundError:
                continue
            if not stat.S_ISREG(file_info.st_mode):
                continue
            os.unlink(entry.path)
            removed += 1
    if removed:
        fsync_dir(directory)
    return removed


def prune_stale_browser_singletons(home: Path) -> int:
    """Remove Chromium's process-local lock symlinks before its profile starts."""
    removed = 0
    browser_dir = home / ".config" / "browser"
    for name in CHROMIUM_SINGLETON_NAMES:
        path = check_safe_parent(home, f".config/browser/{name}")
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        # These links point at a container hostname/PID and a local socket.
        # They cannot describe a live process after this container is recreated.
        # Leave regular files and all browser profile data untouched.
        if stat.S_ISLNK(info.st_mode):
            path.unlink()
            removed += 1
    if removed and browser_dir.is_dir() and not browser_dir.is_symlink():
        fsync_dir(browser_dir)
    return removed


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
    home_info = home.stat()
    owner_uid = 0 if home == DEFAULT_HOME else home_info.st_uid
    owner_gid = 0 if home == DEFAULT_HOME else home_info.st_gid
    try:
        info = state.lstat()
    except FileNotFoundError:
        state.mkdir(mode=0o700)
        os.chown(state, owner_uid, owner_gid)
        info = state.lstat()
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise SafeguardError("profile state path is not a real directory")
    if info.st_uid != owner_uid or stat.S_IMODE(info.st_mode) & 0o077:
        raise SafeguardError("profile state directory ownership or mode is unsafe")
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
        state_info = state.stat()
        os.chown(manifest_path, state_info.st_uid, state_info.st_gid)
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
    # The profile volume survives Host recreation, but Chromium's singleton
    # links refer to the old container's hostname and PID. Run before gem.sh
    # starts Chromium so stale links cannot leave the desktop permanently blank.
    prune_stale_browser_singletons(home)
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
    parser.add_argument(
        "command",
        choices=("prepare", "restore", "workspace-defaults", "secure-workspace-root", "prune-browser-metrics"),
    )
    parser.add_argument("--home", type=Path, default=DEFAULT_HOME, help=argparse.SUPPRESS)
    args = parser.parse_args()
    home = args.home
    try:
        if os.geteuid() != 0 and home == DEFAULT_HOME:
            raise SafeguardError("profile safeguard must run as root")
        if args.command == "prepare":
            prepare(home)
        elif args.command == "workspace-defaults":
            workspace_defaults(home)
        elif args.command == "secure-workspace-root":
            secure_workspace_root(home)
        elif args.command == "prune-browser-metrics":
            print(prune_browser_metrics(home))
        else:
            restore(home)
        return 0
    except Exception as exc:  # Never echo config values or file contents.
        print(f"Morde profile safeguard failed: {type(exc).__name__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
