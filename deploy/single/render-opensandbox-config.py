#!/usr/bin/env python3
"""Bind the OpenSandbox host-path allowlist to the configured Workspace root."""

import argparse
import json
import os
import pathlib
import re
import stat
import tempfile
import tomllib


def canonical_workspace_root(value: str) -> str:
    if not value or not os.path.isabs(value) or "\x00" in value:
        raise ValueError("Workspace current-files root must be an absolute path")
    root = pathlib.Path(value)
    if str(root) != os.path.normpath(value) or not root.is_dir() or root.is_symlink():
        raise ValueError("Workspace current-files root must be an existing canonical directory")
    if str(root.resolve(strict=True)) != value:
        raise ValueError("Workspace current-files root must not contain symlinks")
    return value


def render_config(source: str, workspace_root: str) -> str:
    root = canonical_workspace_root(workspace_root)
    try:
        parsed = tomllib.loads(source)
    except tomllib.TOMLDecodeError as exc:
        raise ValueError("OpenSandbox TOML is invalid") from exc

    storage = parsed.get("storage")
    if storage is not None and not isinstance(storage, dict):
        raise ValueError("OpenSandbox storage config must be a table")

    lines = source.splitlines(keepends=True)
    table = ""
    storage_header = None
    allowlist_line = None
    for index, line in enumerate(lines):
        table_match = re.match(r"^\s*\[([^\]]+)\]\s*(?:#.*)?(?:\r?\n)?$", line)
        if table_match:
            table = table_match.group(1).strip()
            if table == "storage":
                storage_header = index
            continue
        if table == "storage" and re.match(r"^\s*allowed_host_paths\s*=", line):
            if allowlist_line is not None:
                raise ValueError("OpenSandbox storage.allowed_host_paths must be defined once")
            allowlist_line = index

    assignment = f"allowed_host_paths = [{json.dumps(root, ensure_ascii=False)}]"
    if allowlist_line is not None:
        line = lines[allowlist_line]
        prefix = re.match(r"^(\s*allowed_host_paths\s*=\s*)", line)
        if prefix is None or "]" not in line:
            raise ValueError("OpenSandbox storage.allowed_host_paths must use a single-line array")
        newline = "\n" if line.endswith("\n") else ""
        lines[allowlist_line] = prefix.group(1) + f"[{json.dumps(root, ensure_ascii=False)}]" + newline
    elif storage_header is not None:
        lines.insert(storage_header + 1, assignment + "\n")
    else:
        if source and not source.endswith("\n"):
            lines.append("\n")
        if lines and lines[-1].strip():
            lines.append("\n")
        lines.extend(["[storage]\n", assignment + "\n"])

    rendered = "".join(lines)
    try:
        validated = tomllib.loads(rendered)
    except tomllib.TOMLDecodeError as exc:
        raise ValueError("rendered OpenSandbox TOML is invalid") from exc
    validate_rendered_config(validated, root)
    return rendered


def validate_rendered_config(config: dict, workspace_root: str) -> None:
    root = canonical_workspace_root(workspace_root)
    storage = config.get("storage")
    if not isinstance(storage, dict) or storage.get("allowed_host_paths") != [root]:
        raise ValueError("OpenSandbox host-path allowlist must contain only the Workspace current-files root")


def _assert_protected_config(path: pathlib.Path) -> os.stat_result:
    if not path.is_absolute():
        raise ValueError("OpenSandbox config path must be absolute")
    parent = path.parent
    while True:
        info = os.lstat(parent)
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
            raise ValueError("OpenSandbox config parent directories must be root-owned and not group or world writable")
        if parent == pathlib.Path("/"):
            break
        parent = parent.parent
    info = os.lstat(path)
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise ValueError("OpenSandbox config must be a protected root-owned regular file")
    return info


def check_file(config_path: str, workspace_root: str) -> None:
    target = pathlib.Path(config_path)
    _assert_protected_config(target)
    try:
        parsed = tomllib.loads(target.read_text(encoding="utf-8"))
    except tomllib.TOMLDecodeError as exc:
        raise ValueError("OpenSandbox TOML is invalid") from exc
    validate_rendered_config(parsed, workspace_root)


def render_file(config_path: str, workspace_root: str) -> None:
    target = pathlib.Path(config_path)
    info = _assert_protected_config(target)
    source = target.read_text(encoding="utf-8")
    rendered = render_config(source, workspace_root)
    if rendered == source:
        return
    fd, temporary_name = tempfile.mkstemp(prefix=f".{target.name}.", dir=target.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as temporary:
            temporary.write(rendered)
            temporary.flush()
            os.fsync(temporary.fileno())
            os.fchmod(temporary.fileno(), stat.S_IMODE(info.st_mode))
            os.fchown(temporary.fileno(), info.st_uid, info.st_gid)
        os.replace(temporary_name, target)
        directory_fd = os.open(target.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="verify the active allowlist without changing the file")
    parser.add_argument("config", help="protected OpenSandbox TOML config path")
    parser.add_argument("workspace_root", help="configured Workspace current-files root")
    arguments = parser.parse_args()
    try:
        if arguments.check:
            check_file(arguments.config, arguments.workspace_root)
        else:
            render_file(arguments.config, arguments.workspace_root)
    except (OSError, ValueError) as exc:
        parser.error(str(exc))
    print("PASS OpenSandbox host-path allowlist matches the Workspace current-files root")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
