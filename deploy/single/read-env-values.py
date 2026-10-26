#!/usr/bin/env python3
"""Read only non-secret, single-line deployment values without executing the env file."""

import argparse
import io
import os
import re
import stat
import sys


ALLOWED = frozenset({
    "CH_DATA_KEY_FILE",
    "CH_COOKIE_KEY_FILE",
    "CH_WORKSPACE_STORAGE_ROOT_DIR",
    "CH_SITE_STORAGE_ROOT_DIR",
    "CH_POSTGRES_DATA_DIR",
    "CH_DOCKER_HOST",
    "CH_OSB_URL",
    "CH_OSB_CONFIG_FILE",
})
SIMPLE_VALUE = re.compile(r"[A-Za-z0-9_./:@+-]+\Z")
ASSIGNMENT = re.compile(r"\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)\Z")


def parse_values(source: io.TextIOBase, keys: list[str]) -> list[str]:
    requested = set(keys)
    if requested - ALLOWED or len(requested) != len(keys):
        raise ValueError("request unique approved non-secret keys only")
    values: dict[str, str] = {}
    for line_number, raw_line in enumerate(source, 1):
        line = raw_line.rstrip("\n")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        match = ASSIGNMENT.fullmatch(line)
        if not match:
            if any(re.match(rf"\s*{re.escape(key)}\b", line) for key in requested):
                raise ValueError(f"requested key has invalid assignment on line {line_number}")
            continue
        key, value = match.groups()
        if key not in requested:
            continue
        if key in values:
            raise ValueError(f"duplicate requested key {key}")
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if not SIMPLE_VALUE.fullmatch(value):
            raise ValueError(f"{key} must be a simple single-line value")
        values[key] = value

    missing = requested - values.keys()
    if missing:
        raise ValueError("missing required key(s): " + ", ".join(sorted(missing)))
    return [values[key] for key in keys]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--null", action="store_true", help="terminate each value with NUL")
    parser.add_argument("env_file")
    parser.add_argument("keys", nargs="+")
    args = parser.parse_args()
    try:
        info = os.lstat(args.env_file)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o600:
            parser.error("environment file must be root-owned, regular and mode 0600")
        with open(args.env_file, encoding="utf-8") as source:
            values = parse_values(source, args.keys)
    except (OSError, UnicodeError, ValueError) as exc:
        parser.error(str(exc))
    output = ("\0" if args.null else "\n").join(values)
    sys.stdout.write(output + ("\0" if args.null else "\n"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
