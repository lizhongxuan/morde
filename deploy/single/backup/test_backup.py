import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
from pathlib import Path


SCRIPT = Path(__file__).with_name("backup.py")
IMAGE = f"docker.io/library/postgres:16.15-bookworm@sha256:{'a' * 64}"
SPEC = importlib.util.spec_from_file_location("morde_backup", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
BACKUP = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BACKUP)


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def create_fixture(root: Path, *, unsafe_workspace_tar: bool = False) -> Path:
    pg = root / "pg"
    (pg / "global").mkdir(parents=True)
    (pg / "base").mkdir()
    (pg / "PG_VERSION").write_text("16\n", encoding="ascii")
    (pg / "global" / "pg_control").write_bytes(b"synthetic control file")
    (pg / "base" / "synthetic.dat").write_bytes(b"offline test only")

    wal_archive = root / "postgres-wal-archive"
    wal_archive.mkdir(mode=0o700)

    workspace = root / "workspaces"
    workspace_id = workspace / "w1234567"
    workspace_id.mkdir(parents=True)
    block = b"synthetic immutable block"
    (workspace_id / hashlib.sha256(block).hexdigest()).write_bytes(block)

    sites = root / "sites"
    (sites / ".staging").mkdir(parents=True)
    (sites / ".staging" / ".lease.lock").touch(mode=0o600)
    (sites / ".staging" / ".capacity.lock").touch(mode=0o600)
    files = sites / "releases" / "site_0123456789abcdef" / "rel_0123456789abcdef" / "files"
    files.mkdir(parents=True)
    site_content = b"site fixture"
    (files / "index.html").write_bytes(site_content)
    marker = files.parent / ".morde-manifest-sha256"
    marker.write_text("a" * 64 + "\n", encoding="ascii")
    (files / "index.html").chmod(0o444)
    files.chmod(0o500)
    files.parent.chmod(0o500)
    marker.chmod(0o400)
    (sites / ".staging").chmod(0o700)
    (sites / "releases" / "site_0123456789abcdef").chmod(0o700)
    (sites / "releases").chmod(0o700)
    sites.chmod(0o700)

    bundle = root / "backup-bundle"
    bundle.mkdir(mode=0o700)
    with tarfile.open(bundle / "postgres-data.tar", "w") as archive:
        archive.add(pg, arcname=".")
    with tarfile.open(bundle / "postgres-wal-archive.tar", "w") as archive:
        archive.add(wal_archive, arcname=".")
    if unsafe_workspace_tar:
        with tarfile.open(bundle / "workspace-blocks.tar", "w") as archive:
            info = tarfile.TarInfo("../outside")
            info.size = 1
            archive.addfile(info, __import__("io").BytesIO(b"x"))
    else:
        with tarfile.open(bundle / "workspace-blocks.tar", "w") as archive:
            archive.add(workspace, arcname=".")
    with tarfile.open(bundle / "site-artifacts.tar", "w") as archive:
        archive.add(sites, arcname=".")
    metadata = {
        "format": "morde-single-node-backup",
        "format_version": 3,
        "created_at_utc": "2026-01-01T00:00:00+00:00",
        "source_paths": {
            "postgres_data_dir": "/var/lib/postgresql/data",
            "postgres_wal_archive_dir": "/var/lib/postgresql/wal-archive",
            "workspace_root": "/srv/morde/workspaces",
            "site_storage_root": "/srv/morde/sites",
        },
        "postgres": {
            "major_version": "16",
            "image": IMAGE,
            "system_identifier": "7703556091144286253",
            "checkpoint_lsn": "0/2000000",
            "redo_lsn": "0/1FFF000",
            "redo_wal_file": "000000010000000000000001",
            "timeline_id": 1,
            "wal_segment_size": 16777216,
        },
        "postgres_wal_archive": {"segment_count": 0, "total_bytes": 0},
        "workspace_store": {"block_count": 1, "block_bytes": len(block)},
        "site_store": {"release_count": 1, "file_count": 1, "file_bytes": len(site_content)},
        "archives": {
            "postgres_data_dir": "postgres-data.tar",
            "postgres_wal_archive_dir": "postgres-wal-archive.tar",
            "workspace_root": "workspace-blocks.tar",
            "site_storage_root": "site-artifacts.tar",
        },
    }
    (bundle / "metadata.json").write_text(json.dumps(metadata, sort_keys=True, indent=2) + "\n", encoding="utf-8")
    entries = ("postgres-data.tar", "postgres-wal-archive.tar", "workspace-blocks.tar", "site-artifacts.tar", "metadata.json")
    (bundle / "SHA256SUMS").write_text("".join(f"{digest(bundle / name)}  {name}\n" for name in entries), encoding="ascii")
    return bundle


class OfflineBackupValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="morde-backup-test-")
        self.root = Path(self.temp.name)
        self.bundle = create_fixture(self.root)
        (self.root / "workspace-current" / "w1234567").mkdir(parents=True)
        (self.root / "unrelated").mkdir()
        (self.root / "unrelated-volume").mkdir()
        (self.root / "workspaces" / "volume-data").mkdir()
        self.bin_dir = self.root / "bin"
        self.bin_dir.mkdir()
        self.fake_docker = self.bin_dir / "docker"
        self.fake_docker.write_text(
            "#!/bin/sh\n"
            "if [ \"$1 $2\" = \"image inspect\" ]; then exit 0; fi\n"
            "if [ \"$1\" = \"run\" ]; then printf \"Database system identifier: 7703556091144286253\\nDatabase cluster state: shut down\\nLatest checkpoint location: 0/2000000\\nLatest checkpoint's REDO location: 0/1FFF000\\nLatest checkpoint's REDO WAL file: 000000010000000000000001\\nLatest checkpoint's TimeLineID: 1\\nBytes per WAL segment: 16777216\\n\"; exit 0; fi\n"
            "exit 9\n",
            encoding="utf-8",
        )
        self.fake_docker.chmod(0o700)
        self.env = dict(os.environ)
        self.env["PATH"] = f"{self.bin_dir}{os.pathsep}{self.env.get('PATH', '')}"

    def tearDown(self) -> None:
        self.temp.cleanup()

    def run_verify(self, bundle=None) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(SCRIPT), "verify", "--bundle", str(bundle or self.bundle)],
            env=self.env,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_backup_gate_rejects_writable_container_mounts_overlapping_storage(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        for mount_source in (
            self.root / "pg",
            self.root / "workspaces" / "w1234567",
            self.root / "workspace-current" / "w1234567",
            self.root,
        ):
            with self.subTest(mount_source=mount_source), patch.object(
                BACKUP,
                "run",
                side_effect=[
                    "0123456789abcdef\n",
                    json.dumps([{"Type": "bind", "Source": str(mount_source), "RW": True}]),
                ],
            ):
                with self.assertRaisesRegex(BACKUP.BackupError, "writable bind mount overlapping protected storage"):
                    BACKUP.require_no_running_container_mounts(sources)

    def test_backup_gate_rejects_bind_mount_aliases_by_directory_inode(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        cases = (
            ("alias-current", sources["workspace_current_root"]),
            ("alias-parent", self.root),
        )
        original_stat = Path.stat
        for alias_name, target in cases:
            alias = (self.root / alias_name).resolve()
            alias.mkdir()

            def stat_with_bind_alias(path, *args, _alias=alias, _target=target, **kwargs):
                if path == _alias:
                    return original_stat(_target, *args, **kwargs)
                return original_stat(path, *args, **kwargs)

            with self.subTest(alias=alias_name), patch.object(Path, "stat", stat_with_bind_alias), patch.object(
                BACKUP,
                "run",
                side_effect=[
                    "0123456789abcdef\n",
                    json.dumps([{"Type": "bind", "Source": str(alias), "RW": True}]),
                ],
            ):
                with self.assertRaisesRegex(BACKUP.BackupError, "writable bind mount overlapping protected storage"):
                    BACKUP.require_no_running_container_mounts(sources)

    def test_backup_gate_allows_read_only_or_unrelated_container_mounts(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        mounts = [
            {"Type": "bind", "Source": str(self.root / "pg"), "RW": False},
            {"Type": "bind", "Source": str(self.root / "unrelated"), "RW": True},
            {"Type": "volume", "Name": "example", "Source": str(self.root / "unrelated-volume"), "RW": True},
            {"Type": "volume", "Name": "readonly", "Source": "/var/lib/docker/volumes/readonly/_data", "RW": False},
        ]
        with patch.object(BACKUP, "run", side_effect=[
            "0123456789abcdef\n", json.dumps(mounts),
            json.dumps({"Driver": "local", "Mountpoint": str(self.root / "unrelated-volume"), "Options": None}),
        ]):
            BACKUP.require_no_running_container_mounts(sources)
        with patch.object(BACKUP, "run", return_value=""):
            BACKUP.require_no_running_container_mounts(sources)

    def test_backup_gate_rejects_named_volume_and_local_bind_volume_overlapping_storage(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        cases = (
            (
                {"Driver": "local", "Mountpoint": str(self.root / "workspaces" / "volume-data"), "Options": None},
                str(self.root / "workspaces" / "volume-data"),
            ),
            (
                {
                    "Driver": "local",
                    "Mountpoint": str(self.root / "docker-volumes" / "custom"),
                    "Options": {"type": "none", "device": str(self.root / "sites"), "o": "bind,rw"},
                },
                str(self.root / "docker-volumes" / "custom"),
            ),
        )
        for volume, source in cases:
            with self.subTest(volume=volume), patch.object(
                BACKUP,
                "run",
                side_effect=[
                    "0123456789abcdef\n",
                    json.dumps([{"Type": "volume", "Name": "custom", "Source": source, "RW": True}]),
                    json.dumps(volume),
                ],
            ):
                with self.assertRaisesRegex(BACKUP.BackupError, "writable named volume overlapping protected storage"):
                    BACKUP.require_no_running_container_mounts(sources)

    def test_backup_gate_rejects_unverifiable_writable_volume_backends(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        mount = {"Type": "volume", "Name": "custom", "Source": "/var/lib/docker/volumes/custom/_data", "RW": True}
        for volume, message in (
            ({"Driver": "nfs", "Mountpoint": mount["Source"], "Options": {}}, "volume driver cannot be verified"),
            ({"Driver": "local", "Mountpoint": mount["Source"], "Options": {"type": "nfs", "device": "server:/share", "o": "rw"}}, "volume options cannot be verified"),
        ):
            with self.subTest(volume=volume), patch.object(
                BACKUP,
                "run",
                side_effect=["0123456789abcdef\n", json.dumps([mount]), json.dumps(volume)],
            ):
                with self.assertRaisesRegex(BACKUP.BackupError, message):
                    BACKUP.require_no_running_container_mounts(sources)

    def test_backup_gate_fails_closed_on_unreadable_or_unsupported_mount_metadata(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        with patch.object(BACKUP, "run", side_effect=["0123456789abcdef\n", "not-json"]):
            with self.assertRaisesRegex(BACKUP.BackupError, "could not inspect running Docker container mounts"):
                BACKUP.require_no_running_container_mounts(sources)
        with patch.object(BACKUP, "run", side_effect=["0123456789abcdef\n", json.dumps([{"Type": "bind", "Source": "relative", "RW": True}])]):
            with self.assertRaisesRegex(BACKUP.BackupError, "invalid running container mount source"):
                BACKUP.require_no_running_container_mounts(sources)
        with patch.object(BACKUP, "run", side_effect=["0123456789abcdef\n", json.dumps([{"Type": "image", "RW": False}])]):
            BACKUP.require_no_running_container_mounts(sources)
        with patch.object(BACKUP, "run", side_effect=["0123456789abcdef\n", json.dumps([{"Type": "image", "RW": True}])]):
            with self.assertRaisesRegex(BACKUP.BackupError, "unsupported writable container mount data"):
                BACKUP.require_no_running_container_mounts(sources)

    def test_service_stop_gate_includes_running_container_mount_check(self) -> None:
        sources = {
            "postgres_data_dir": (self.root / "pg").resolve(),
            "workspace_root": (self.root / "workspaces").resolve(),
            "workspace_current_root": (self.root / "workspace-current").resolve(),
            "site_storage_root": (self.root / "sites").resolve(),
        }
        with (
            patch.object(BACKUP, "require_portal_stopped") as portal_stopped,
            patch.object(BACKUP, "require_postgres_stopped") as postgres_stopped,
            patch.object(BACKUP, "require_no_running_container_mounts") as mounts_stopped,
            patch.object(BACKUP, "compose_postgres_image", return_value=IMAGE),
        ):
            self.assertEqual(BACKUP.check_services_stopped(Path("env"), Path("compose"), sources), IMAGE)
        portal_stopped.assert_called_once_with()
        postgres_stopped.assert_called_once_with(Path("env"), Path("compose"))
        mounts_stopped.assert_called_once_with(sources)

    def test_current_root_must_not_overlap_archive_roots(self) -> None:
        nested_current = self.root / "workspaces" / "current"
        nested_current.mkdir()
        args = type("Args", (), {
            "postgres_data_dir": str(self.root / "pg"),
            "postgres_wal_archive_dir": str(self.root / "postgres-wal-archive"),
            "workspace_root": str(self.root / "workspaces"),
            "workspace_current_root": str(nested_current),
            "site_storage_root": str(self.root / "sites"),
        })()
        with self.assertRaisesRegex(BACKUP.BackupError, "directories must not overlap"):
            BACKUP.source_paths(args)

    def test_wal_archive_must_not_overlap_postgres_data(self) -> None:
        wal_archive = self.root / "pg" / "wal-archive"
        wal_archive.mkdir()
        args = type("Args", (), {
            "postgres_data_dir": str(self.root / "pg"),
            "postgres_wal_archive_dir": str(wal_archive),
            "workspace_root": str(self.root / "workspaces"),
            "workspace_current_root": str(self.root / "workspace-current"),
            "site_storage_root": str(self.root / "sites"),
        })()
        with self.assertRaisesRegex(BACKUP.BackupError, "directories must not overlap"):
            BACKUP.source_paths(args)

    def test_recovery_can_resolve_missing_data_targets_from_existing_parents(self) -> None:
        target_root = self.root / "interrupted"
        target_root.mkdir()
        args = type("Args", (), {
            "postgres_data_dir": str(target_root / "pg"),
            "postgres_wal_archive_dir": str(target_root / "wal"),
            "workspace_root": str(target_root / "blocks"),
            "workspace_current_root": str(self.root / "workspace-current"),
            "site_storage_root": str(target_root / "sites"),
        })()
        sources = BACKUP.source_paths(args, allow_missing=True)
        self.assertFalse(sources["postgres_data_dir"].exists())
        self.assertFalse(sources["postgres_wal_archive_dir"].exists())

    def test_verifies_synthetic_postgres_workspace_and_site_bundle(self) -> None:
        result = self.run_verify()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Backup integrity verified", result.stdout)
        self.assertIn("Workspace blocks: 1", result.stdout)
        self.assertIn("Site releases: 1", result.stdout)

    def test_pg_controldata_runs_as_control_file_owner(self) -> None:
        for uid, gid in ((999, 999), (0, 0)):
            with self.subTest(uid=uid, gid=gid):
                info = os.stat_result((BACKUP.stat.S_IFREG | 0o600, 0, 0, 1, uid, gid, 0, 0, 0, 0))
                with patch.object(BACKUP.os, "lstat", return_value=info), patch.object(
                    BACKUP, "run", side_effect=["", "Database cluster state: shut down\n"]
                ) as run:
                    BACKUP.postgres_control_data(self.root / "pg", IMAGE)

                args = run.call_args_list[-1].args[0]
                self.assertEqual(args[args.index("--user") + 1], f"{uid}:{gid}")

    def test_pg_controldata_rejects_a_non_regular_control_file(self) -> None:
        info = os.stat_result((BACKUP.stat.S_IFLNK | 0o777, 0, 0, 1, 999, 999, 0, 0, 0, 0))
        with patch.object(BACKUP.os, "lstat", return_value=info), patch.object(BACKUP, "run") as run:
            with self.assertRaisesRegex(BACKUP.BackupError, "not a regular file"):
                BACKUP.postgres_control_data(self.root / "pg", IMAGE)
        run.assert_not_called()

    def test_wal_archive_accepts_postgres_backup_history_files(self) -> None:
        wal_archive = self.root / "private-wal"
        wal_archive.mkdir(mode=0o700)
        os.chmod(wal_archive, 0o700)
        (wal_archive / "000000010000000000000001").write_bytes(b"w" * (1 << 20))
        (wal_archive / "000000010000000000000001.00000020.backup").write_text("backup history\n", encoding="ascii")
        for path in wal_archive.iterdir():
            path.chmod(0o600)

        self.assertEqual(BACKUP.validate_wal_archive(wal_archive, 1 << 20), (1, (1 << 20) + len("backup history\n")))

    def test_follow_up_requires_a_later_lsn_from_the_same_system_and_timeline(self) -> None:
        base = json.loads((self.bundle / "metadata.json").read_text(encoding="utf-8"))["postgres"]
        follow_up = {**base, "checkpoint_lsn": "0/3000000"}
        self.assertEqual(
            BACKUP.wal_replay_segments(base, follow_up, 4),
            [
                "000000010000000000000001",
                "000000010000000000000002",
                "000000010000000000000003",
            ],
        )
        with self.assertRaisesRegex(BACKUP.BackupError, "different PostgreSQL system"):
            BACKUP.wal_replay_segments(base, {**follow_up, "system_identifier": "7703556091144286254"}, 4)
        with self.assertRaisesRegex(BACKUP.BackupError, "different PostgreSQL timeline"):
            BACKUP.wal_replay_segments(base, {**follow_up, "timeline_id": 2}, 4)
        with self.assertRaisesRegex(BACKUP.BackupError, "missing one or more required segments"):
            BACKUP.wal_replay_segments(base, follow_up, 1)

    def test_follow_up_replay_requires_every_wal_segment_and_uses_target_pg_wal_tail(self) -> None:
        base = {
            "system_identifier": "1",
            "checkpoint_lsn": "0/180000",
            "redo_lsn": "0/80000",
            "timeline_id": 1,
            "wal_segment_size": 1 << 20,
        }
        follow_up = {**base, "checkpoint_lsn": "0/300000"}
        wal_stage = self.root / "follow-up-wal"
        wal_stage.mkdir(mode=0o700)
        for segment in range(3):
            name = f"00000001000000000000000{segment}"
            target = wal_stage / name
            target.write_bytes(bytes([segment]) * (1 << 20))
            target.chmod(0o600)
        postgres_tar = self.root / "follow-up-postgres.tar"
        with tarfile.open(postgres_tar, "w") as archive:
            member = tarfile.TarInfo("pg_wal/000000010000000000000003")
            member.size = 1 << 20
            archive.addfile(member, __import__("io").BytesIO(b"\x03" * (1 << 20)))
        segments = BACKUP.prepare_follow_up_wal(base, follow_up, wal_stage, postgres_tar)
        self.assertEqual(len(segments), 4)
        self.assertEqual((wal_stage / segments[-1]).stat().st_size, 1 << 20)
        self.assertEqual(digest(wal_stage / segments[-1]), hashlib.sha256(b"\x03" * (1 << 20)).hexdigest())

    def test_rejects_tampered_site_archive_before_validation(self) -> None:
        with (self.bundle / "site-artifacts.tar").open("ab") as target:
            target.write(b"tampered")
        result = self.run_verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum verification failed for site-artifacts.tar", result.stderr)

    def test_rejects_unrecognized_site_staging_entry(self) -> None:
        root = self.root / "unsafe-site-staging"
        bundle = create_fixture(root)
        sites = root / "sites"
        (sites / ".staging" / "unexpected").write_text("keep", encoding="utf-8")
        with tarfile.open(bundle / "site-artifacts.tar", "w") as archive:
            archive.add(sites, arcname=".")
        entries = ("postgres-data.tar", "postgres-wal-archive.tar", "workspace-blocks.tar", "site-artifacts.tar", "metadata.json")
        (bundle / "SHA256SUMS").write_text(
            "".join(f"{digest(bundle / name)}  {name}\n" for name in entries),
            encoding="ascii",
        )
        result = self.run_verify(bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Site staging contains an unrecognized entry", result.stderr)

    def test_rejects_tampered_archive_before_validation(self) -> None:
        with (self.bundle / "workspace-blocks.tar").open("ab") as target:
            target.write(b"tampered")
        result = self.run_verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum verification failed", result.stderr)

    def test_rejects_path_traversal_even_with_recomputed_checksums(self) -> None:
        unsafe_bundle = create_fixture(self.root / "unsafe", unsafe_workspace_tar=True)
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "verify", "--bundle", str(unsafe_bundle)],
            env=self.env,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe path", result.stderr)
        self.assertFalse((self.root / "outside").exists())

    def test_rejects_duplicate_archive_paths(self) -> None:
        with tarfile.open(self.bundle / "workspace-blocks.tar", "w") as archive:
            for name in (".", "./."):
                directory = tarfile.TarInfo(name)
                directory.type = tarfile.DIRTYPE
                archive.addfile(directory)
        entries = ("postgres-data.tar", "postgres-wal-archive.tar", "workspace-blocks.tar", "site-artifacts.tar", "metadata.json")
        (self.bundle / "SHA256SUMS").write_text(
            "".join(f"{digest(self.bundle / name)}  {name}\n" for name in entries),
            encoding="ascii",
        )
        result = self.run_verify()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("duplicate paths", result.stderr)

    def test_rejects_symlink_to_bundle(self) -> None:
        link = self.root / "bundle-link"
        link.symlink_to(self.bundle, target_is_directory=True)
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "verify", "--bundle", str(link)],
            env=self.env,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot be a symlink", result.stderr)

    def test_recovery_requires_explicit_confirmation_flag(self) -> None:
        args = type("Args", (), {"confirm_recover": False})()
        with self.assertRaisesRegex(BACKUP.BackupError, "--confirm-recover"):
            BACKUP.confirm_recovery(args, self.root / "pg", self.root / "workspaces", self.root / "sites")

    def test_recovery_rejects_incorrect_typed_confirmation(self) -> None:
        pgdata = self.root / "pg"
        workspace = self.root / "workspaces"
        args = type("Args", (), {"confirm_recover": True})()

        class InteractiveInput:
            @staticmethod
            def isatty() -> bool:
                return True

        with patch.object(BACKUP.sys, "stdin", InteractiveInput()), patch("builtins.input", return_value="ROLLBACK wrong"):
            with self.assertRaisesRegex(BACKUP.BackupError, "did not match"):
                BACKUP.confirm_recovery(args, pgdata, workspace, self.root / "sites")

    def test_restore_failure_reinstates_original_directories(self) -> None:
        target = self.root / "pg-target"
        hold = self.root / ".pg-target.pre-restore-test"
        stage = self.root / ".pg-target.morde-restore-test"
        target.mkdir()
        (target / "restored-data").write_text("new", encoding="utf-8")
        hold.mkdir()
        (hold / "original-data").write_text("old", encoding="utf-8")
        failures = BACKUP.restore_original_paths([(target, hold)], [(target, stage)])
        self.assertEqual(failures, [])
        self.assertEqual((target / "original-data").read_text(encoding="utf-8"), "old")
        self.assertEqual((stage / "restored-data").read_text(encoding="utf-8"), "new")

    def test_recover_interrupted_restore_keeps_new_data_and_restores_old_pair(self) -> None:
        targets = [self.root / "pg-target", self.root / "workspace-target", self.root / "site-target"]
        holds = [
            self.root / ".pg-target.pre-restore-test",
            self.root / ".workspace-target.pre-restore-test",
            self.root / ".site-target.pre-restore-test",
        ]
        paths = []
        for target, hold, label in zip(targets, holds, ("pg", "workspace", "site")):
            target.mkdir()
            (target / "new").write_text(f"new-{label}", encoding="utf-8")
            hold.mkdir()
            (hold / "old").write_text(f"old-{label}", encoding="utf-8")
            archive_path = self.root / f"{label}-original.tar"
            with tarfile.open(archive_path, "w") as archive:
                archive.add(hold, arcname=".")
            paths.append((label, target, hold, self.root / f".{target.name}.morde-restore-test", archive_path))

        first_recovery_interrupted = BACKUP.recover_original_directories([paths[0]])
        retry_interrupted = BACKUP.recover_original_directories(paths)
        self.assertEqual(len(first_recovery_interrupted), 1)
        self.assertEqual(len(retry_interrupted), 2)
        for target, label in zip(targets, ("pg", "workspace", "site")):
            self.assertEqual((target / "old").read_text(encoding="utf-8"), f"old-{label}")
        self.assertCountEqual(
            sorted(path.read_text(encoding="utf-8") for item in first_recovery_interrupted + retry_interrupted for path in item.iterdir()),
            ["new-pg", "new-workspace", "new-site"],
        )

    def test_recovery_fails_closed_when_hold_is_missing_and_target_is_ambiguous(self) -> None:
        target = self.root / "pg-target"
        target.mkdir()
        (target / "unknown").write_text("keep", encoding="utf-8")
        reference = self.root / "original.tar"
        with tarfile.open(reference, "w") as archive:
            info = tarfile.TarInfo("old")
            info.size = 3
            archive.addfile(info, io.BytesIO(b"old"))
        paths = [("postgres_data_dir", target, self.root / ".pg-target.pre-restore-test", self.root / ".stage", reference)]
        with self.assertRaisesRegex(BACKUP.BackupError, "does not match"):
            BACKUP.recover_original_directories(paths)
        self.assertEqual((target / "unknown").read_text(encoding="utf-8"), "keep")

    def test_ambiguous_recovery_records_manual_inspection_and_preserves_target(self) -> None:
        target = self.root / "pg-target"
        target.mkdir()
        (target / "unknown").write_text("keep", encoding="utf-8")
        reference = self.root / "original.tar"
        with tarfile.open(reference, "w") as archive:
            info = tarfile.TarInfo("old")
            info.size = 3
            archive.addfile(info, io.BytesIO(b"old"))

        journal = self.root / "RESTORE-JOURNAL.json"
        state = {"format": "morde-single-restore-journal", "status": "recovering_original_data"}
        BACKUP.write_json_private(journal, state)
        paths = [("postgres_data_dir", target, self.root / ".pg-target.pre-restore-test", self.root / ".stage", reference)]

        with self.assertRaisesRegex(BACKUP.BackupError, "manual_recovery_required"):
            BACKUP.validate_recovery_candidates(paths, journal, state)

        recorded = json.loads(journal.read_text(encoding="utf-8"))
        self.assertEqual(recorded["status"], "manual_recovery_required")
        self.assertIn("inspect", recorded["manual_recovery_reason"])
        self.assertEqual((target / "unknown").read_text(encoding="utf-8"), "keep")


if __name__ == "__main__":
    unittest.main()
