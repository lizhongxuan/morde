#!/usr/bin/env python3
"""Add repository-declared model hosts to an installed Portal config."""

import argparse
import ipaddress
import os
import re
import stat
import sys
import tempfile
from pathlib import Path


ALLOWLIST_LINE = re.compile(r"^(?P<prefix>  allowed_hosts:\s*)\[(?P<items>[^\]\r\n]*)\](?P<tail>[ \t]*(?:#.*)?)$")
HOST_LABEL = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$")


def _parse_host_list(value: str, source: str) -> list[str]:
    hosts = []
    for raw in value.split(","):
        host = raw.strip()
        if len(host) >= 2 and host[0] == host[-1] and host[0] in "\"'":
            host = host[1:-1]
        labels = host.lower().split(".")
        if not host or len(host) > 253 or len(labels) < 2 or any(
            not label or len(label) > 63 or not HOST_LABEL.fullmatch(label) for label in labels
        ):
            raise ValueError(f"{source}: invalid model host")
        try:
            ipaddress.ip_address(host)
        except ValueError:
            pass
        else:
            raise ValueError(f"{source}: model host must be a DNS name, not an IP literal")
        hosts.append(host.lower())
    if not hosts:
        raise ValueError(f"{source}: model host allowlist must not be empty")
    if len(set(hosts)) != len(hosts):
        raise ValueError(f"{source}: duplicate model host")
    return hosts


def _allowlist_line(text: str, source: str) -> tuple[list[str], int, re.Match[str], str, list[str]]:
    lines = text.splitlines(keepends=True)
    models_sections = [index for index, line in enumerate(lines) if re.fullmatch(r"models:\s*(?:#.*)?\r?\n?", line)]
    if len(models_sections) != 1:
        raise ValueError(f"{source}: expected one top-level models section")
    start = models_sections[0]
    end = len(lines)
    for index in range(start + 1, len(lines)):
        if re.match(r"^[^\s#][^:]*:", lines[index]):
            end = index
            break
    matches = []
    for index in range(start + 1, end):
        line = lines[index].rstrip("\r\n")
        if re.match(r"^  allowed_hosts\s*:", line):
            match = ALLOWLIST_LINE.fullmatch(line)
            if not match:
                raise ValueError(f"{source}: allowed_hosts must use an inline DNS host list")
            newline = lines[index][len(line):]
            matches.append((index, match, newline))
    if len(matches) != 1:
        raise ValueError(f"{source}: expected one models.allowed_hosts entry")
    index, match, newline = matches[0]
    return _parse_host_list(match.group("items"), source), index, match, newline, lines


def merge_model_hosts(source_text: str, target_text: str) -> tuple[str, list[str]]:
    source_hosts, _, _, _, _ = _allowlist_line(source_text, "source config")
    current_hosts, index, match, newline, lines = _allowlist_line(target_text, "active config")
    known = {host.lower() for host in current_hosts}
    added = [host for host in source_hosts if host.lower() not in known]
    if not added:
        return target_text, []

    # shortcut: this sync only adds repository defaults, so remove retired hosts from the active config manually.
    merged = [*current_hosts, *added]
    body = f"{match.group('prefix')}[{', '.join(merged)}]{match.group('tail')}{newline}"
    lines[index] = body
    return "".join(lines), added


def _atomic_write(path: Path, content: str) -> None:
    if path.is_symlink():
        raise ValueError("active config must not be a symlink")
    original = path.stat()
    if not stat.S_ISREG(original.st_mode):
        raise ValueError("active config must be a regular file")
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as output:
            os.fchmod(output.fileno(), stat.S_IMODE(original.st_mode))
            os.fchown(output.fileno(), original.st_uid, original.st_gid)
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group(required=True)
    action.add_argument("--check", action="store_true", help="check whether active config includes source hosts")
    action.add_argument("--sync", action="store_true", help="add missing source hosts to active config")
    parser.add_argument("source", type=Path)
    parser.add_argument("target", type=Path)
    args = parser.parse_args()
    try:
        if args.target.is_symlink():
            raise ValueError("active config must not be a symlink")
        updated, added = merge_model_hosts(args.source.read_text(encoding="utf-8"), args.target.read_text(encoding="utf-8"))
        if args.check:
            if added:
                print(f"active model allowlist is missing {len(added)} repository host(s)")
                return 1
            return 0
        if added:
            _atomic_write(args.target, updated)
            print(f"added {len(added)} repository model host(s) to the active config")
        return 0
    except (OSError, UnicodeError, ValueError) as error:
        print(str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
