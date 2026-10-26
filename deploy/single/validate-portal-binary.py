#!/usr/bin/env python3
"""Reject Portal binaries that cannot run on the target Linux architecture."""

import pathlib
import sys


ELF_MACHINES = {"x86_64": 62, "aarch64": 183}


def validate_elf(path: pathlib.Path, architecture: str) -> None:
    expected_machine = ELF_MACHINES.get(architecture)
    if expected_machine is None:
        raise ValueError(f"unsupported Linux architecture: {architecture}")

    try:
        with open(path, "rb") as binary:
            header = binary.read(20)
    except OSError as error:
        raise ValueError(f"cannot read Portal binary: {error}") from error

    if len(header) < 20 or header[:4] != b"\x7fELF" or header[4] != 2 or header[5] != 1:
        raise ValueError("Portal binary must be ELF64 little-endian")

    actual_machine = int.from_bytes(header[18:20], "little")
    if actual_machine != expected_machine:
        raise ValueError(f"Portal binary architecture does not match {architecture}")


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print("usage: validate-portal-binary.py <binary> <uname-architecture>", file=sys.stderr)
        return 2
    try:
        validate_elf(pathlib.Path(argv[1]), argv[2])
    except ValueError as error:
        print(f"Portal binary: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
