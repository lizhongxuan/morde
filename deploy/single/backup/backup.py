#!/usr/bin/env python3
"""Offline, local-only backup and same-path restore for a single Morde node."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import uuid
from pathlib import Path, PurePosixPath
from typing import Any


FORMAT = "morde-single-node-backup"
FORMAT_VERSION = 2
MAX_WORKSPACE_BLOCK_BYTES = 4 * 1024 * 1024
DOCKER_BIN = "docker"
SCRIPT_DIR = Path(__file__).resolve().parent
DEFAULT_COMPOSE_FILE = SCRIPT_DIR.parent / "docker-compose.yml"
LOCAL_FILESYSTEMS = {"ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "f2fs"}
WORKSPACE_ID = re.compile(r"^w[a-z0-9]{7}$")
BLOCK_DIGEST = re.compile(r"^[0-9a-f]{64}$")
SITE_ID = re.compile(r"^site_[0-9a-f]{16}$")
RELEASE_ID = re.compile(r"^rel_[0-9a-f]{16}$")
SITE_MARKER = ".morde-manifest-sha256"
SITE_STAGING_LOCKS = {".lease.lock", ".capacity.lock"}
BACKUP_TARGETS = (
    ("postgres_data_dir", "postgres-data.tar"),
    ("workspace_root", "workspace-blocks.tar"),
    ("site_storage_root", "site-artifacts.tar"),
)


class BackupError(Exception):
    pass


def run(args: list[str], *, input_text: str | None = None) -> str:
    try:
        result = subprocess.run(
            args,
            input=input_text,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
    except OSError as exc:
        raise BackupError(f"required command is unavailable: {Path(args[0]).name}") from exc
    if result.returncode != 0:
        # Command output can contain deployment details or credentials. Never
        # echo it into terminal logs or metadata.
        raise BackupError(f"command failed: {Path(args[0]).name} (exit {result.returncode})")
    return result.stdout


def canonical_dir(value: str, label: str, *, must_exist: bool = True) -> Path:
    raw = Path(value).expanduser()
    if not raw.is_absolute() or raw == Path("/"):
        raise BackupError(f"{label} must be an absolute non-root path")
    if must_exist:
        try:
            info = raw.lstat()
        except OSError as exc:
            raise BackupError(f"cannot inspect {label}") from exc
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise BackupError(f"{label} must be a real directory, not a symlink")
    try:
        resolved = raw.resolve(strict=must_exist)
    except OSError as exc:
        raise BackupError(f"cannot resolve {label}") from exc
    if resolved == Path("/"):
        raise BackupError(f"{label} cannot be the filesystem root")
    if must_exist:
        info = resolved.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise BackupError(f"{label} must be a real directory, not a symlink")
    return resolved


def is_within(parent: Path, child: Path) -> bool:
    try:
        child.relative_to(parent)
        return True
    except ValueError:
        return False


def reject_overlapping_paths(*paths: Path) -> None:
    for index, path in enumerate(paths):
        if any(is_within(path, other) or is_within(other, path) for other in paths[index + 1 :]):
            raise BackupError("PostgreSQL, Workspace, and Site storage directories must not overlap")


def ensure_local_filesystem(path: Path, label: str) -> None:
    try:
        filesystem = run(["findmnt", "--noheadings", "--output", "FSTYPE", "--target", str(path)]).strip()
    except BackupError as exc:
        raise BackupError(f"cannot verify local filesystem for {label}; install util-linux findmnt") from exc
    if filesystem not in LOCAL_FILESYSTEMS:
        raise BackupError(f"{label} uses unsupported or non-local filesystem type {filesystem!r}")


def reject_symlink_or_special_tree(root: Path, label: str) -> None:
    for directory, dirnames, filenames in os.walk(root, topdown=True, followlinks=False):
        for name in list(dirnames):
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if not stat.S_ISDIR(mode):
                raise BackupError(f"{label} contains a symlink or non-directory entry")
        for name in filenames:
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if not stat.S_ISREG(mode):
                raise BackupError(f"{label} contains a symlink or special file")


def reject_nested_mounts(root: Path, label: str) -> None:
    for directory, dirnames, _ in os.walk(root, topdown=True, followlinks=False):
        for name in dirnames:
            if os.path.ismount(Path(directory) / name):
                raise BackupError(f"{label} contains a nested mount point; refusing a partial archive")


def validate_workspace_store(root: Path) -> tuple[int, int]:
    reject_symlink_or_special_tree(root, "workspace store")
    block_count = 0
    total_bytes = 0
    for workspace_entry in root.iterdir():
        if not WORKSPACE_ID.fullmatch(workspace_entry.name) or not workspace_entry.is_dir():
            raise BackupError(f"workspace store contains an unrecognized entry: {workspace_entry.name!r}")
        for block in workspace_entry.iterdir():
            if not BLOCK_DIGEST.fullmatch(block.name) or not block.is_file():
                raise BackupError(f"workspace {workspace_entry.name} contains an unrecognized block entry")
            size = block.stat().st_size
            if size > MAX_WORKSPACE_BLOCK_BYTES:
                raise BackupError(f"workspace block exceeds {MAX_WORKSPACE_BLOCK_BYTES} bytes")
            digest = hashlib.sha256()
            with block.open("rb") as source:
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    digest.update(chunk)
            if digest.hexdigest() != block.name:
                raise BackupError(f"workspace block checksum mismatch in workspace {workspace_entry.name}")
            block_count += 1
            total_bytes += size
    return block_count, total_bytes


def validate_site_store(root: Path) -> tuple[int, int, int]:
    reject_symlink_or_special_tree(root, "Site artifact store")
    if {entry.name for entry in root.iterdir()} != {".staging", "releases"}:
        raise BackupError("Site artifact store must contain only .staging and releases")
    staging = root / ".staging"
    releases_root = root / "releases"
    if not staging.is_dir() or not releases_root.is_dir():
        raise BackupError("Site artifact store is missing its staging or releases directory")
    for directory in (root, root / ".staging", root / "releases"):
        mode = directory.stat().st_mode
        if mode & 0o077 or mode & 0o700 != 0o700:
            raise BackupError("Site artifact store directories must be private to the Portal account")
    for site_dir in staging.iterdir():
        if site_dir.name in SITE_STAGING_LOCKS:
            if not site_dir.is_file() or stat.S_IMODE(site_dir.stat().st_mode) != 0o600:
                raise BackupError("Site staging lock files must be private regular files")
            continue
        if not SITE_ID.fullmatch(site_dir.name) or not site_dir.is_dir():
            raise BackupError("Site staging contains an unrecognized entry")
        mode = site_dir.stat().st_mode
        if mode & 0o077 or mode & 0o700 != 0o700:
            raise BackupError("Site staging directories must be private to the Portal account")
        for stage_dir in site_dir.iterdir():
            if not re.fullmatch(r"\.tmp-[0-9a-f]{32}", stage_dir.name) or not stage_dir.is_dir():
                raise BackupError("Site staging contains an unrecognized release entry")
            mode = stage_dir.stat().st_mode
            if mode & 0o077 or mode & 0o700 != 0o700:
                raise BackupError("Site staging directories must be private to the Portal account")
    release_count = 0
    file_count = 0
    file_bytes = 0
    for site_dir in releases_root.iterdir():
        if not SITE_ID.fullmatch(site_dir.name) or not site_dir.is_dir():
            raise BackupError("Site release store contains an unrecognized site entry")
        for release_dir in site_dir.iterdir():
            if not RELEASE_ID.fullmatch(release_dir.name) or not release_dir.is_dir():
                raise BackupError("Site release store contains an unrecognized release entry")
            if {entry.name for entry in release_dir.iterdir()} != {"files", SITE_MARKER}:
                raise BackupError("Site release is missing its files or manifest marker")
            files_root = release_dir / "files"
            marker = release_dir / SITE_MARKER
            if not files_root.is_dir() or not marker.is_file():
                raise BackupError("Site release is incomplete")
            try:
                marker_data = marker.read_bytes()
                digest = marker_data[:-1].decode("ascii") if marker_data.endswith(b"\n") else ""
            except (OSError, UnicodeError) as exc:
                raise BackupError("Site release manifest marker is unreadable") from exc
            if not BLOCK_DIGEST.fullmatch(digest) or len(marker_data) != 65:
                raise BackupError("Site release manifest marker is invalid")
            if any(path.stat().st_mode & 0o222 for path in (release_dir, files_root, marker)):
                raise BackupError("Site release contains writable published entries")
            for directory, dirnames, filenames in os.walk(files_root, topdown=True, followlinks=False):
                parent = Path(directory)
                if parent.stat().st_mode & 0o222:
                    raise BackupError("Site release contains writable published directories")
                for name in dirnames:
                    if (parent / name).stat().st_mode & 0o222:
                        raise BackupError("Site release contains writable published directories")
                for name in filenames:
                    file = parent / name
                    if file.stat().st_mode & 0o222:
                        raise BackupError("Site release contains writable published files")
                    file_count += 1
                    file_bytes += file.stat().st_size
            release_count += 1
    return release_count, file_count, file_bytes


def postgres_major_from_image(image: str) -> str:
    match = re.search(r"postgres:([0-9]+)(?=[.\-@])", image)
    if not match or "@sha256:" not in image:
        raise BackupError("PostgreSQL image must use an explicit major version and sha256 digest")
    return match.group(1)


def validate_postgres_tree(root: Path, expected_major: str | None = None) -> str:
    reject_symlink_or_special_tree(root, "PostgreSQL data directory")
    version_file = root / "PG_VERSION"
    control_file = root / "global" / "pg_control"
    if not version_file.is_file() or not control_file.is_file():
        raise BackupError("PostgreSQL data directory is missing PG_VERSION or global/pg_control")
    major = version_file.read_text(encoding="ascii").strip().split(".", 1)[0]
    if not major.isdigit() or (expected_major is not None and major != expected_major):
        raise BackupError("PostgreSQL data directory version does not match the expected major version")
    return major


def compose_prefix(env_file: Path, compose_file: Path) -> list[str]:
    if not env_file.is_file() or not compose_file.is_file():
        raise BackupError("compose env file or compose file is missing")
    return [DOCKER_BIN, "compose", "--env-file", str(env_file), "-f", str(compose_file)]


def compose_postgres_image(env_file: Path, compose_file: Path) -> str:
    try:
        config = json.loads(run(compose_prefix(env_file, compose_file) + ["config", "--format", "json"]))
        image = config["services"]["postgres"]["image"]
    except (KeyError, json.JSONDecodeError, TypeError) as exc:
        raise BackupError("could not read the PostgreSQL image from Compose configuration") from exc
    if not isinstance(image, str):
        raise BackupError("Compose PostgreSQL image is invalid")
    postgres_major_from_image(image)
    return image


def require_postgres_stopped(env_file: Path, compose_file: Path) -> None:
    ids = run(compose_prefix(env_file, compose_file) + ["ps", "--all", "--quiet", "postgres"]).split()
    if not ids:
        raise BackupError("Compose has no PostgreSQL container; refusing to assume the database is stopped")
    for container_id in ids:
        state = run([DOCKER_BIN, "inspect", "--format", "{{.State.Status}}", container_id]).strip()
        if state != "exited":
            raise BackupError("PostgreSQL container is not cleanly stopped; stop it manually before backup or restore")
    # Also catch a manually started host PostgreSQL server that is unrelated to
    # this Compose project and might use the same data directory.
    processes = run(["ps", "-eo", "args="])
    if re.search(r"(?:^|\s)(?:\S*/)?postgres(?:\s|$)", processes, re.MULTILINE):
        raise BackupError("a PostgreSQL process is still running; stop it manually before backup or restore")


def require_portal_stopped() -> None:
    processes = run(["ps", "-eo", "args="])
    if re.search(r"(?:^|\s)(?:\S*/)?portal\s+serve(?:\s|$)", processes, re.MULTILINE):
        raise BackupError("Portal is still running; stop portal serve manually before backup or restore")


def require_no_running_container_mounts(sources: dict[str, Path]) -> None:
    for container_id in run([DOCKER_BIN, "ps", "--quiet"]).split():
        if not re.fullmatch(r"[0-9a-fA-F]{12,64}", container_id):
            raise BackupError("Docker returned an invalid running container identifier")
        try:
            mounts = json.loads(run([DOCKER_BIN, "inspect", "--format", "{{json .Mounts}}", container_id]))
        except json.JSONDecodeError as exc:
            raise BackupError("could not inspect running Docker container mounts") from exc
        if not isinstance(mounts, list):
            raise BackupError("Docker returned invalid running container mounts")
        for mount in mounts:
            if not isinstance(mount, dict):
                raise BackupError("Docker returned invalid running container mount data")
            mount_type = mount.get("Type")
            writable = mount.get("RW")
            if not isinstance(mount_type, str) or type(writable) is not bool:
                raise BackupError("Docker returned unsupported running container mount data")
            if not writable or mount_type == "tmpfs":
                continue
            if mount_type not in {"bind", "volume"}:
                raise BackupError("Docker returned unsupported writable container mount data")
            source = mount.get("Source")
            if not isinstance(source, str) or not Path(source).is_absolute():
                raise BackupError("Docker returned invalid running container mount source")
            try:
                mounted_path = Path(source).resolve(strict=False)
            except (OSError, RuntimeError) as exc:
                raise BackupError("Docker returned an unresolvable running container mount path") from exc
            if mount_type == "volume":
                name = mount.get("Name")
                if not isinstance(name, str) or not name:
                    raise BackupError("Docker returned invalid named volume data")
                try:
                    volume = json.loads(run([DOCKER_BIN, "volume", "inspect", "--format", "{{json .}}", name]))
                except json.JSONDecodeError as exc:
                    raise BackupError("could not inspect running Docker volume") from exc
                if not isinstance(volume, dict) or volume.get("Driver") != "local":
                    raise BackupError("writable Docker volume driver cannot be verified for offline backup")
                mountpoint = volume.get("Mountpoint")
                options = volume.get("Options")
                if not isinstance(mountpoint, str) or not Path(mountpoint).is_absolute() or not isinstance(options, (dict, type(None))):
                    raise BackupError("Docker returned invalid named volume metadata")
                try:
                    volume_path = Path(mountpoint).resolve(strict=False)
                except (OSError, RuntimeError) as exc:
                    raise BackupError("Docker returned an unresolvable named volume path") from exc
                if volume_path != mounted_path:
                    raise BackupError("Docker named volume mountpoint does not match the running container source")
                if options:
                    if set(options) != {"type", "device", "o"} or options.get("type") != "none" or not isinstance(options.get("o"), str) or "bind" not in options["o"].split(","):
                        raise BackupError("writable Docker local volume options cannot be verified for offline backup")
                    device = options.get("device")
                    if not isinstance(device, str) or not Path(device).is_absolute():
                        raise BackupError("Docker returned invalid local volume device path")
                    try:
                        mounted_path = Path(device).resolve(strict=False)
                    except (OSError, RuntimeError) as exc:
                        raise BackupError("Docker returned an unresolvable local volume device path") from exc
            if any(is_within(root, mounted_path) or is_within(mounted_path, root) for root in sources.values()):
                mount_label = "bind mount" if mount_type == "bind" else "named volume"
                raise BackupError(
                    f"running Docker container {container_id[:12]} has a writable {mount_label} overlapping protected storage; "
                    "stop it manually before backup or restore"
                )


def check_services_stopped(env_file: Path, compose_file: Path, sources: dict[str, Path]) -> str:
    require_portal_stopped()
    require_postgres_stopped(env_file, compose_file)
    require_no_running_container_mounts(sources)
    return compose_postgres_image(env_file, compose_file)


def require_image_available(image: str) -> None:
    run([DOCKER_BIN, "image", "inspect", image])


def postgres_control_data(root: Path, image: str) -> str:
    if "," in str(root):
        raise BackupError("PostgreSQL data path cannot contain commas for a safe read-only Docker mount")
    require_image_available(image)
    args = [
        DOCKER_BIN,
        "run",
        "--rm",
        "--pull=never",
        "--network",
        "none",
        "--read-only",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--mount",
        f"type=bind,src={root},dst=/data,readonly",
        "--entrypoint",
        "pg_controldata",
        image,
        "/data",
    ]
    output = run(args)
    state = re.search(r"^Database cluster state:\s*(.+?)\s*$", output, re.MULTILINE)
    if not state or state.group(1) != "shut down":
        raise BackupError("PostgreSQL cluster is not verified cleanly shut down")
    return output


def source_paths(args: argparse.Namespace) -> dict[str, Path]:
    pgdata = canonical_dir(args.postgres_data_dir, "PostgreSQL data directory")
    workspace = canonical_dir(args.workspace_root, "workspace block store")
    site = canonical_dir(args.site_storage_root, "Site artifact store")
    reject_overlapping_paths(pgdata, workspace, site)
    return {"postgres_data_dir": pgdata, "workspace_root": workspace, "site_storage_root": site}


def bundle_path(path_text: str) -> Path:
    path = Path(path_text).expanduser()
    if not path.is_absolute() or path == Path("/"):
        raise BackupError("backup bundle path must be an absolute non-root path")
    if path.is_symlink():
        raise BackupError("backup bundle path cannot be a symlink")
    return path.resolve(strict=False)


def ensure_bundle_outside_sources(bundle: Path, sources: dict[str, Path]) -> None:
    if any(is_within(source, bundle) or is_within(bundle, source) for source in sources.values()):
        raise BackupError("backup bundle must be outside all source directories")


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def make_tar(source: Path, destination: Path) -> None:
    require_gnu_tar()
    run([
        "tar",
        "--create",
        "--file",
        str(destination),
        "--format=pax",
        "--numeric-owner",
        "--xattrs",
        "--acls",
        "--selinux",
        "--sparse",
        "--one-file-system",
        "--directory",
        str(source),
        ".",
    ])
    os.chmod(destination, 0o600)
    with destination.open("rb") as archive:
        os.fsync(archive.fileno())


def write_json_private(path: Path, value: dict[str, Any]) -> None:
    data = json.dumps(value, sort_keys=True, indent=2) + "\n"
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as target:
        target.write(data)
        target.flush()
        os.fsync(target.fileno())


def write_json_replace(path: Path, value: dict[str, Any]) -> None:
    temporary = path.with_name(path.name + ".tmp-" + uuid.uuid4().hex)
    write_json_private(temporary, value)
    os.replace(temporary, path)
    os.chmod(path, 0o600)
    fsync_directory(path.parent)


def create_bundle(bundle: Path, sources: dict[str, Path], image: str) -> None:
    if bundle.exists():
        raise BackupError("backup bundle already exists; refusing to overwrite it")
    ensure_local_filesystem(bundle.parent.resolve(strict=True), "backup destination")
    for key, path in sources.items():
        ensure_local_filesystem(path, key)
        if os.path.ismount(path):
            raise BackupError("data directory roots cannot themselves be mount points; same-path restore requires sibling renames")
    pgdata = sources["postgres_data_dir"]
    workspace = sources["workspace_root"]
    site = sources["site_storage_root"]
    reject_nested_mounts(pgdata, "PostgreSQL data directory")
    reject_nested_mounts(workspace, "workspace store")
    reject_nested_mounts(site, "Site artifact store")
    block_count, block_bytes = validate_workspace_store(workspace)
    release_count, site_file_count, site_file_bytes = validate_site_store(site)
    pg_major = postgres_major_from_image(image)
    validate_postgres_tree(pgdata, pg_major)
    postgres_control_data(pgdata, image)

    bundle.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    bundle.mkdir(mode=0o700)
    os.chmod(bundle, 0o700)
    try:
        for key, archive in BACKUP_TARGETS:
            make_tar(sources[key], bundle / archive)
        metadata = {
            "format": FORMAT,
            "format_version": FORMAT_VERSION,
            "created_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
            "source_paths": {key: str(path) for key, path in sources.items()},
            "postgres": {"major_version": pg_major, "image": image},
            "workspace_store": {"block_count": block_count, "block_bytes": block_bytes},
            "site_store": {"release_count": release_count, "file_count": site_file_count, "file_bytes": site_file_bytes},
            "archives": {key: archive for key, archive in BACKUP_TARGETS},
        }
        write_json_private(bundle / "metadata.json", metadata)
        entries = [archive for _, archive in BACKUP_TARGETS] + ["metadata.json"]
        sums = "".join(f"{file_sha256(bundle / name)}  {name}\n" for name in entries)
        fd = os.open(bundle / "SHA256SUMS", os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w", encoding="ascii") as target:
            target.write(sums)
            target.flush()
            os.fsync(target.fileno())
        os.chmod(bundle / "SHA256SUMS", 0o600)
        fsync_directory(bundle)
    except Exception:
        shutil.rmtree(bundle, ignore_errors=True)
        raise


def fsync_directory(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def load_and_verify_manifest(bundle: Path) -> dict[str, Any]:
    if bundle.is_symlink() or not bundle.is_dir():
        raise BackupError("backup bundle must be a real directory")
    archive_names = {archive for _, archive in BACKUP_TARGETS}
    required = archive_names | {"metadata.json", "SHA256SUMS"}
    for name in required:
        target = bundle / name
        try:
            info = target.lstat()
        except OSError as exc:
            raise BackupError(f"backup bundle is missing {name}") from exc
        if not stat.S_ISREG(info.st_mode):
            raise BackupError(f"backup bundle entry {name} is not a regular file")
    try:
        lines = (bundle / "SHA256SUMS").read_text(encoding="ascii").splitlines()
    except (OSError, UnicodeError) as exc:
        raise BackupError("backup checksum manifest is unreadable") from exc
    found: dict[str, str] = {}
    checksum_names = "|".join(re.escape(name) for name in archive_names | {"metadata.json"})
    for line in lines:
        match = re.fullmatch(rf"([0-9a-f]{{64}})  ({checksum_names})", line)
        if not match or match.group(2) in found:
            raise BackupError("backup checksum manifest is malformed")
        found[match.group(2)] = match.group(1)
    if set(found) != archive_names | {"metadata.json"}:
        raise BackupError("backup checksum manifest has missing or extra entries")
    for name, expected in found.items():
        if file_sha256(bundle / name) != expected:
            raise BackupError(f"backup checksum verification failed for {name}")
    try:
        metadata = json.loads((bundle / "metadata.json").read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise BackupError("backup metadata is invalid") from exc
    if not isinstance(metadata, dict) or metadata.get("format") != FORMAT or metadata.get("format_version") != FORMAT_VERSION:
        raise BackupError("unsupported backup format")
    if metadata.get("archives") != {key: archive for key, archive in BACKUP_TARGETS}:
        raise BackupError("backup archive metadata is invalid")
    paths = metadata.get("source_paths")
    postgres = metadata.get("postgres")
    store = metadata.get("workspace_store")
    site_store = metadata.get("site_store")
    if not isinstance(paths, dict) or not isinstance(postgres, dict) or not isinstance(store, dict) or not isinstance(site_store, dict):
        raise BackupError("backup source metadata is incomplete")
    if set(paths) != {key for key, _ in BACKUP_TARGETS}:
        raise BackupError("backup source paths are incomplete")
    source_paths = []
    for key, _ in BACKUP_TARGETS:
        value = paths.get(key)
        if not isinstance(value, str) or not Path(value).is_absolute() or Path(value) == Path("/"):
            raise BackupError("backup source paths are invalid")
        source_paths.append(Path(value))
    reject_overlapping_paths(*source_paths)
    if any(type(site_store.get(key)) is not int or site_store[key] < 0 for key in ("release_count", "file_count", "file_bytes")):
        raise BackupError("backup Site artifact metadata is invalid")
    image = postgres.get("image")
    major = postgres.get("major_version")
    if not isinstance(image, str) or postgres_major_from_image(image) != str(major):
        raise BackupError("backup PostgreSQL metadata is invalid")
    return metadata


def require_private_bundle(bundle: Path) -> None:
    directory = bundle.lstat()
    if directory.st_uid != 0 or stat.S_IMODE(directory.st_mode) & 0o077:
        raise BackupError("destructive operations require a root-owned backup directory with no group/other access")
    for name in [archive for _, archive in BACKUP_TARGETS] + ["metadata.json", "SHA256SUMS"]:
        info = (bundle / name).lstat()
        if info.st_uid != 0 or stat.S_IMODE(info.st_mode) & 0o077:
            raise BackupError(f"destructive operations require root-owned private bundle files: {name}")


def validate_archive(path: Path) -> None:
    try:
        with tarfile.open(path, "r:") as archive:
            seen: set[tuple[str, ...]] = set()
            for member in archive:
                name = member.name
                pure = PurePosixPath(name)
                if pure.is_absolute() or any(part == ".." for part in pure.parts):
                    raise BackupError("backup archive contains an unsafe path")
                normalized = tuple(part for part in pure.parts if part not in ("", "."))
                if normalized in seen:
                    raise BackupError("backup archive contains duplicate paths")
                seen.add(normalized)
                if not (member.isdir() or member.isfile()):
                    raise BackupError("backup archive contains a symlink, hardlink, or special file")
    except (tarfile.TarError, OSError) as exc:
        raise BackupError("backup tar archive is invalid") from exc


def require_gnu_tar() -> None:
    version = run(["tar", "--version"])
    if not version.startswith("tar (GNU tar)"):
        raise BackupError("backup and restore require GNU tar to preserve Linux ownership, ACLs, and xattrs")


def extract_tar(archive: Path, destination: Path, *, preserve_owner: bool) -> None:
    if preserve_owner:
        require_gnu_tar()
        run([
            "tar",
            "--extract",
            "--file",
            str(archive),
            "--directory",
            str(destination),
            "--numeric-owner",
            "--xattrs",
            "--acls",
            "--selinux",
            "--delay-directory-restore",
            "--same-owner",
        ])
        return
    # Bundle verification also runs on developer Macs. Use Python's tar reader
    # for safe regular-file extraction there; actual creation/restore require
    # GNU tar and preserve the Linux metadata.
    directory_modes: list[tuple[Path, int]] = []
    with tarfile.open(archive, "r:") as source:
        for member in source:
            parts = [part for part in PurePosixPath(member.name).parts if part not in ("", ".")]
            target = destination.joinpath(*parts) if parts else destination
            if not is_within(destination, target):
                raise BackupError("backup archive contains an unsafe path")
            if member.isdir():
                if target != destination:
                    target.mkdir(parents=True, exist_ok=True)
                directory_modes.append((target, member.mode & 0o777))
                continue
            if not parts:
                raise BackupError("backup archive contains a file at the archive root")
            target.parent.mkdir(parents=True, exist_ok=True)
            content = source.extractfile(member)
            if content is None:
                raise BackupError("backup archive contains an unreadable file")
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, member.mode & 0o777)
            with os.fdopen(descriptor, "wb") as output, content:
                shutil.copyfileobj(content, output)
    for path, mode in sorted(directory_modes, key=lambda entry: len(entry[0].parts), reverse=True):
        os.chmod(path, mode)


def verify_bundle(bundle: Path) -> dict[str, Any]:
    metadata = load_and_verify_manifest(bundle)
    postgres_tar = bundle / "postgres-data.tar"
    workspace_tar = bundle / "workspace-blocks.tar"
    site_tar = bundle / "site-artifacts.tar"
    validate_archive(postgres_tar)
    validate_archive(workspace_tar)
    validate_archive(site_tar)
    with tempfile.TemporaryDirectory(prefix="morde-backup-verify-") as temporary:
        root = Path(temporary)
        pg_stage = root / "postgres"
        workspace_stage = root / "workspace"
        site_stage = root / "site"
        pg_stage.mkdir(mode=0o700)
        workspace_stage.mkdir(mode=0o700)
        site_stage.mkdir(mode=0o700)
        extract_tar(postgres_tar, pg_stage, preserve_owner=False)
        extract_tar(workspace_tar, workspace_stage, preserve_owner=False)
        extract_tar(site_tar, site_stage, preserve_owner=False)
        major = validate_postgres_tree(pg_stage, str(metadata["postgres"]["major_version"]))
        blocks, block_bytes = validate_workspace_store(workspace_stage)
        releases, site_file_count, site_file_bytes = validate_site_store(site_stage)
        expected_store = metadata["workspace_store"]
        if expected_store.get("block_count") != blocks or expected_store.get("block_bytes") != block_bytes:
            raise BackupError("workspace store totals do not match backup metadata")
        expected_site = metadata["site_store"]
        if expected_site != {"release_count": releases, "file_count": site_file_count, "file_bytes": site_file_bytes}:
            raise BackupError("Site artifact totals do not match backup metadata")
        postgres_control_data(pg_stage, str(metadata["postgres"]["image"]))
        if major != str(metadata["postgres"]["major_version"]):
            raise BackupError("PostgreSQL major version does not match backup metadata")
    return metadata


def directory_matches_archive(root: Path, archive_path: Path) -> bool:
    if not stat.S_ISDIR(root.lstat().st_mode):
        return False
    reject_symlink_or_special_tree(root, "recovery directory")
    expected: dict[tuple[str, ...], tuple[str, int, str] | tuple[str]] = {}
    validate_archive(archive_path)
    with tarfile.open(archive_path, "r:") as archive:
        for member in archive:
            key = tuple(part for part in PurePosixPath(member.name).parts if part not in ("", "."))
            if member.isdir():
                expected[key] = ("dir",)
                continue
            content = archive.extractfile(member)
            if content is None:
                return False
            digest = hashlib.sha256()
            size = 0
            with content:
                for chunk in iter(lambda: content.read(1024 * 1024), b""):
                    digest.update(chunk)
                    size += len(chunk)
            expected[key] = ("file", size, digest.hexdigest())

    actual: dict[tuple[str, ...], tuple[str, int, str] | tuple[str]] = {(): ("dir",)}
    for directory, dirnames, filenames in os.walk(root, topdown=True, followlinks=False):
        parent = Path(directory)
        for name in dirnames:
            key = tuple((parent / name).relative_to(root).parts)
            actual[key] = ("dir",)
        for name in filenames:
            path = parent / name
            digest = file_sha256(path)
            actual[tuple(path.relative_to(root).parts)] = ("file", path.stat().st_size, digest)
    return actual == expected


def mark_manual_recovery_required(journal: Path, state: dict[str, Any], reason: str) -> None:
    state["status"] = "manual_recovery_required"
    state["manual_recovery_reason"] = reason
    write_json_replace(journal, state)


def validate_recovery_candidates(
    paths: list[tuple[str, Path, Path, Path, Path]], journal: Path, state: dict[str, Any]
) -> None:
    try:
        for _, target, hold, _, archive_path in paths:
            if not target.exists() and not hold.exists():
                raise BackupError("a target and its pre-restore sibling are both missing")
            for candidate in (target, hold):
                if candidate.exists() and not stat.S_ISDIR(candidate.lstat().st_mode):
                    raise BackupError("a recovery path is not a real directory")
            preserved = hold if hold.exists() else target
            if not directory_matches_archive(preserved, archive_path):
                raise BackupError("the preserved directory does not match the verified pre-restore data")
    except (BackupError, OSError) as exc:
        reason = "automatic recovery could not prove the original directory trees; inspect targets, sibling directories, and the rollback bundle manually"
        try:
            mark_manual_recovery_required(journal, state, reason)
        except OSError as journal_exc:
            raise BackupError("automatic recovery is unsafe; manual inspection is required, but the journal could not be updated") from journal_exc
        raise BackupError(f"{exc}; journal status is manual_recovery_required; inspect all recovery paths manually") from exc


def get_args_paths(args: argparse.Namespace) -> tuple[Path, Path, dict[str, Path]]:
    env_file = Path(args.env_file).expanduser().resolve(strict=True)
    compose_file = Path(args.compose_file).expanduser().resolve(strict=True)
    return env_file, compose_file, source_paths(args)


def backup_command(args: argparse.Namespace) -> None:
    require_root()
    env_file, compose_file, sources = get_args_paths(args)
    image = check_services_stopped(env_file, compose_file, sources)
    bundle = bundle_path(args.destination)
    ensure_bundle_outside_sources(bundle, sources)
    create_bundle(bundle, sources, image)
    verify_bundle(bundle)
    print(f"Backup created and verified: {bundle}")
    print("Files are local, mode 0600; bundle directory mode is 0700.")


def verify_command(args: argparse.Namespace) -> None:
    metadata = verify_bundle(bundle_path(args.bundle))
    print("Backup integrity verified.")
    print(f"PostgreSQL major: {metadata['postgres']['major_version']}")
    print(f"Workspace blocks: {metadata['workspace_store']['block_count']}")
    print(f"Site releases: {metadata['site_store']['release_count']}")
    print("This verifies bundle integrity; it does not connect to or change a live service.")


def validate_live_sources(sources: dict[str, Path], image: str, major: str) -> None:
    if postgres_major_from_image(image) != validate_postgres_tree(sources["postgres_data_dir"], major):
        raise BackupError("live PostgreSQL directory major version does not match the backup")
    validate_workspace_store(sources["workspace_root"])
    validate_site_store(sources["site_storage_root"])
    postgres_control_data(sources["postgres_data_dir"], image)
    if any(os.path.ismount(path) for path in sources.values()):
        raise BackupError("data directory roots cannot themselves be mount points; same-path restore requires sibling renames")


def validate_staged_source(key: str, path: Path, image: str, major: str) -> None:
    if key == "postgres_data_dir":
        validate_postgres_tree(path, major)
        postgres_control_data(path, image)
    elif key == "workspace_root":
        validate_workspace_store(path)
    else:
        validate_site_store(path)


def new_restore_paths(sources: dict[str, Path], bundle: Path) -> list[tuple[str, Path, Path, Path, Path]]:
    paths: list[tuple[str, Path, Path, Path, Path]] = []
    try:
        for key, archive in BACKUP_TARGETS:
            target = sources[key]
            stage = Path(tempfile.mkdtemp(prefix=f".{target.name}.morde-restore-", dir=target.parent))
            hold = target.with_name(f".{target.name}.pre-restore-{uuid.uuid4().hex}")
            paths.append((key, target, hold, stage, bundle / archive))
    except Exception:
        for _, _, _, stage, _ in paths:
            shutil.rmtree(stage, ignore_errors=True)
        raise
    return paths


def fsync_target_parents(paths: list[tuple[str, Path, Path, Path, Path]]) -> None:
    for parent in dict.fromkeys(target.parent for _, target, _, _, _ in paths):
        fsync_directory(parent)


def restore_command(args: argparse.Namespace) -> None:
    require_root()
    bundle = bundle_path(args.bundle)
    require_private_bundle(bundle)
    metadata = verify_bundle(bundle)
    env_file, compose_file, sources = get_args_paths(args)
    ensure_bundle_outside_sources(bundle, sources)
    if {key: str(path) for key, path in sources.items()} != metadata["source_paths"]:
        raise BackupError("restore requires the exact PostgreSQL, Workspace, and Site paths recorded in the backup")
    image = check_services_stopped(env_file, compose_file, sources)
    if image != metadata["postgres"]["image"]:
        raise BackupError("restore requires the exact digest-pinned PostgreSQL image recorded in the backup")
    validate_live_sources(sources, image, str(metadata["postgres"]["major_version"]))
    confirm_restore(args, *(sources[key] for key, _ in BACKUP_TARGETS))

    rollback_text = args.rollback_dir or str(bundle.parent / f"{bundle.name}.pre-restore.{timestamp()}")
    rollback_bundle = bundle_path(rollback_text)
    ensure_bundle_outside_sources(rollback_bundle, sources)
    if rollback_bundle == bundle or is_within(bundle, rollback_bundle) or is_within(rollback_bundle, bundle):
        raise BackupError("rollback bundle must be separate from the selected backup bundle")
    if rollback_bundle.exists():
        raise BackupError("rollback bundle already exists; refusing to overwrite it")
    create_bundle(rollback_bundle, sources, image)
    verify_bundle(rollback_bundle)

    paths = new_restore_paths(sources, bundle)
    journal = rollback_bundle / "RESTORE-JOURNAL.json"
    state: dict[str, Any] = {
        "format": "morde-single-restore-journal",
        "status": "staging",
        "backup_bundle": str(bundle),
        "rollback_bundle": str(rollback_bundle),
        "targets": {key: str(target) for key, target in sources.items()},
        "temporary_paths": {
            **{f"{key}_stage": str(stage) for key, _, _, stage, _ in paths},
            **{f"{key}_before_restore": str(hold) for key, _, hold, _, _ in paths},
        },
    }
    moved: list[tuple[Path, Path]] = []
    installed: list[tuple[Path, Path]] = []
    try:
        write_json_private(journal, state)
        for key, _, _, stage, archive in paths:
            extract_tar(archive, stage, preserve_owner=True)
            validate_staged_source(key, stage, image, str(metadata["postgres"]["major_version"]))
        state["status"] = "ready_to_swap"
        write_json_replace(journal, state)

        for _, target, hold, _, _ in paths:
            os.rename(target, hold)
            moved.append((target, hold))
        fsync_target_parents(paths)
        state["status"] = "old_data_preserved_at_sibling_paths"
        write_json_replace(journal, state)
        for _, target, _, stage, _ in paths:
            os.rename(stage, target)
            installed.append((target, stage))
        fsync_target_parents(paths)
        for key, target, _, _, _ in paths:
            validate_staged_source(key, target, image, str(metadata["postgres"]["major_version"]))
        state["status"] = "restored"
        write_json_replace(journal, state)
    except Exception as exc:
        state["status"] = "failed_restore_original_data_retained"
        try:
            write_json_replace(journal, state)
        except OSError:
            pass
        rollback_failures = restore_original_paths(moved, installed)
        if rollback_failures:
            raise BackupError(
                "restore failed and automatic rollback could not restore every original directory; "
                "keep the rollback bundle and sibling directories, then use recover after inspection"
            ) from exc
        raise
    finally:
        for _, _, _, stage, _ in paths:
            if stage.exists():
                shutil.rmtree(stage, ignore_errors=True)

    # Keep the verified rollback bundle as the durable pre-restore copy. Remove
    # sibling names only after all restored directories pass validation.
    for _, _, hold, _, _ in paths:
        shutil.rmtree(hold)
    fsync_target_parents(paths)
    print("Restore completed and verified.")
    print(f"Pre-restore rollback bundle: {rollback_bundle}")


def recover_command(args: argparse.Namespace) -> None:
    require_root()
    rollback_bundle = bundle_path(args.rollback_bundle)
    require_private_bundle(rollback_bundle)
    metadata = verify_bundle(rollback_bundle)
    env_file, compose_file, sources = get_args_paths(args)
    ensure_bundle_outside_sources(rollback_bundle, sources)
    expected_paths = metadata["source_paths"]
    if {key: str(path) for key, path in sources.items()} != expected_paths:
        raise BackupError("recovery requires the exact PostgreSQL, Workspace, and Site paths recorded in the rollback bundle")
    image = check_services_stopped(env_file, compose_file, sources)
    if image != metadata["postgres"]["image"]:
        raise BackupError("recovery requires the exact digest-pinned PostgreSQL image recorded in the rollback bundle")

    journal = rollback_bundle / "RESTORE-JOURNAL.json"
    try:
        journal_info = journal.lstat()
        if not stat.S_ISREG(journal_info.st_mode):
            raise BackupError("restore journal is not a regular file")
        state = json.loads(journal.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise BackupError("rollback bundle has no readable interrupted-restore journal") from exc
    if not isinstance(state, dict) or state.get("format") != "morde-single-restore-journal":
        raise BackupError("restore journal format is invalid")
    if state.get("status") == "restored":
        print("The journal confirms that restore completed. The verified rollback bundle remains available.")
        return
    if state.get("status") == "recovered_original_data":
        print("The journal confirms that original data was already recovered.")
        return
    if state.get("status") == "manual_recovery_required":
        raise BackupError("journal status is manual_recovery_required; inspect the targets, sibling directories, and rollback bundle before further recovery")
    allowed_statuses = {"staging", "ready_to_swap", "old_data_preserved_at_sibling_paths", "failed_restore_original_data_retained", "recovering_original_data"}
    if state.get("status") not in allowed_statuses:
        raise BackupError("restore journal has an unknown state; refusing automatic recovery")
    target_record = state.get("targets")
    temp_record = state.get("temporary_paths")
    if target_record != {key: str(path) for key, path in sources.items()} or not isinstance(temp_record, dict):
        raise BackupError("restore journal target paths do not match the selected deployment")

    paths: list[tuple[str, Path, Path, Path, Path]] = []
    for key, archive in BACKUP_TARGETS:
        target = sources[key]
        hold_text = temp_record.get(f"{key}_before_restore")
        stage_text = temp_record.get(f"{key}_stage")
        if not isinstance(hold_text, str) or not isinstance(stage_text, str):
            raise BackupError("restore journal temporary paths are incomplete")
        hold = Path(hold_text)
        stage = Path(stage_text)
        if hold.parent.resolve(strict=False) != target.parent or not hold.name.startswith(f".{target.name}.pre-restore-"):
            raise BackupError("restore journal contains an unsafe rollback sibling path")
        if stage.parent.resolve(strict=False) != target.parent or not stage.name.startswith(f".{target.name}.morde-restore-"):
            raise BackupError("restore journal contains an unsafe staging sibling path")
        for candidate in (hold, stage):
            if candidate.exists():
                info = candidate.lstat()
                if not stat.S_ISDIR(info.st_mode):
                    raise BackupError("restore journal sibling path is not a real directory")
        paths.append((key, target, hold, stage, rollback_bundle / archive))

    # Validate recoverability before moving anything. Ambiguous trees are
    # journaled as manual-only so a later retry cannot guess which tree is old.
    validate_recovery_candidates(paths, journal, state)
    confirm_recovery(args, *(sources[key] for key, _ in BACKUP_TARGETS))

    state["status"] = "recovering_original_data"
    write_json_replace(journal, state)
    try:
        recover_original_directories(paths)
        validate_live_sources(sources, image, str(metadata["postgres"]["major_version"]))
    except (BackupError, OSError) as exc:
        reason = "automatic recovery stopped during filesystem replacement or validation; inspect all targets, sibling directories, and interrupted-restore directories manually"
        try:
            mark_manual_recovery_required(journal, state, reason)
        except OSError as journal_exc:
            raise BackupError("automatic recovery stopped; manual inspection is required, but the journal could not be updated") from journal_exc
        raise BackupError("automatic recovery stopped; journal status is manual_recovery_required; inspect all recovery paths manually") from exc
    state["status"] = "recovered_original_data"
    write_json_replace(journal, state)
    print("Original PostgreSQL, Workspace, and Site data were restored from their pre-restore sibling directories.")
    print("Any interrupted restored data was retained under .interrupted-restore-* for manual inspection.")


def restore_original_paths(moved: list[tuple[Path, Path]], installed: list[tuple[Path, Path]]) -> list[Path]:
    failures: list[Path] = []
    for target, stage in reversed(installed):
        if target.exists() and not stage.exists():
            try:
                os.rename(target, stage)
            except OSError:
                failures.append(target)
    for target, hold in reversed(moved):
        if not hold.exists():
            continue
        if target.exists():
            failures.append(hold)
            continue
        try:
            os.rename(hold, target)
            fsync_directory(target.parent)
        except OSError:
            failures.append(hold)
    return failures


def recover_original_directories(paths: list[tuple[str, Path, Path, Path, Path]]) -> list[Path]:
    interrupted: list[Path] = []
    for _, target, hold, _, archive_path in paths:
        if not hold.exists():
            if not target.exists() or not directory_matches_archive(target, archive_path):
                raise BackupError("target does not match the pre-restore data after its sibling disappeared")
            continue
        if not directory_matches_archive(hold, archive_path):
            raise BackupError("preserved sibling no longer matches the pre-restore data")
        if target.exists():
            preserved = target.with_name(f".{target.name}.interrupted-restore-{uuid.uuid4().hex}")
            os.rename(target, preserved)
            interrupted.append(preserved)
        os.rename(hold, target)
        fsync_directory(target.parent)
    return interrupted


def confirm_restore(args: argparse.Namespace, *targets: Path) -> None:
    if not args.confirm_restore:
        raise BackupError("restore is destructive; pass --confirm-restore and type the exact confirmation phrase")
    if not sys.stdin.isatty():
        raise BackupError("restore confirmation requires an interactive terminal")
    phrase = "RESTORE " + " ".join(str(path) for path in targets)
    print(f"This replaces all three data directories. Type exactly:\n{phrase}", flush=True)
    try:
        entered = input("Confirm: ")
    except EOFError as exc:
        raise BackupError("restore confirmation was not provided") from exc
    if entered != phrase:
        raise BackupError("restore confirmation did not match the exact target paths")


def confirm_recovery(args: argparse.Namespace, *targets: Path) -> None:
    if not args.confirm_recover:
        raise BackupError("recovery can replace interrupted data; pass --confirm-recover and type the exact confirmation phrase")
    if not sys.stdin.isatty():
        raise BackupError("recovery confirmation requires an interactive terminal")
    phrase = "ROLLBACK " + " ".join(str(path) for path in targets)
    print(f"This restores the preserved pre-restore directories. Type exactly:\n{phrase}", flush=True)
    try:
        entered = input("Confirm: ")
    except EOFError as exc:
        raise BackupError("recovery confirmation was not provided") from exc
    if entered != phrase:
        raise BackupError("recovery confirmation did not match the exact target paths")


def require_root() -> None:
    if os.name != "posix" or not sys.platform.startswith("linux"):
        raise BackupError("backup and restore require a Linux host")
    if os.geteuid() != 0:
        raise BackupError("backup and restore require root to preserve PostgreSQL ownership and permissions")


def timestamp() -> str:
    return dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def add_target_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--env-file", default="/etc/cloudhost/cloudhost.env")
    parser.add_argument("--compose-file", default=str(DEFAULT_COMPOSE_FILE))
    parser.add_argument("--postgres-data-dir", required=True)
    parser.add_argument("--workspace-root", required=True)
    parser.add_argument("--site-storage-root", required=True)


def make_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Offline local backup and restore for Morde single-node deployments")
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("create", help="create a local offline backup")
    add_target_args(create)
    create.add_argument("--destination", required=True, help="new absolute bundle directory; it must not already exist")
    verify = commands.add_parser("verify", help="verify bundle checksums and stored data")
    verify.add_argument("--bundle", required=True)
    restore = commands.add_parser("restore", help="restore a verified backup to its original paths")
    add_target_args(restore)
    restore.add_argument("--bundle", required=True)
    restore.add_argument("--rollback-dir", help="new local directory for a verified pre-restore copy")
    restore.add_argument("--confirm-restore", action="store_true", help="also requires typing the exact three target paths")
    recover = commands.add_parser("recover", help="roll back an interrupted restore using its sibling data and rollback journal")
    add_target_args(recover)
    recover.add_argument("--rollback-bundle", required=True, help="rollback bundle printed by restore")
    recover.add_argument("--confirm-recover", action="store_true", help="also requires typing the exact three target paths")
    return parser


def main(argv: list[str] | None = None) -> int:
    os.umask(0o077)
    args = make_parser().parse_args(argv)
    try:
        if args.command == "create":
            backup_command(args)
        elif args.command == "verify":
            verify_command(args)
        elif args.command == "recover":
            recover_command(args)
        else:
            restore_command(args)
        return 0
    except BackupError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    except (OSError, tarfile.TarError, subprocess.SubprocessError) as exc:
        print(f"ERROR: safe operation failed ({type(exc).__name__}); inspect local paths and logs before retrying", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
