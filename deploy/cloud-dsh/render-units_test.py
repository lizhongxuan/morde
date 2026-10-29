#!/usr/bin/env python3
"""Safe local checks for cloud-dsh unit rendering."""

import json
import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parent
SCRIPT = ROOT / "render-units.sh"
EXPECTED = {
    "dsh-docker-daemon.json",
    "morde-dsh-containerd.service",
    "morde-dsh-docker.service",
    "morde-dsh-guardian.service",
}


class RenderUnitsTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.temp_root = pathlib.Path(self.temp.name)
        self.bin_dir = self.temp_root / "bin"
        self.bin_dir.mkdir()
        self.output = self.temp_root / "out"
        self.output.mkdir()
        self.install_account_stubs()
        self.fixtures = {
            "FAKE_PASSWD_ENTRIES": (
                "portal_test:x:1001:2001::/nonexistent:/usr/sbin/nologin\n"
                "nobody:x:65534:65534::/nonexistent:/usr/sbin/nologin"
            ),
            "FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:portal_test\nnogroup:x:65534:nobody",
            "FAKE_ID_USER": "portal_test",
            "FAKE_ID_UID": "1001",
        }

    def install_account_stubs(self):
        getent = self.bin_dir / "getent"
        getent.write_text(
            "#!/usr/bin/env python3\n"
            "import os, sys\n"
            "args = sys.argv[1:]\n"
            "if len(args) not in (3, 4) or args[:2] != ['-s', 'files']:\n"
            "    raise SystemExit(2)\n"
            "database, = args[2:3]\n"
            "entries = os.environ.get('FAKE_' + database.upper() + '_ENTRIES', '').splitlines()\n"
            "if len(args) == 4:\n"
            "    entries = [line for line in entries if line.split(':', 1)[0] == args[3]]\n"
            "if not entries:\n"
            "    raise SystemExit(2)\n"
            "print('\\n'.join(entries))\n"
        )
        getent.chmod(0o755)
        id_command = self.bin_dir / "id"
        id_command.write_text(
            "#!/usr/bin/env python3\n"
            "import os, sys\n"
            "if sys.argv[1:] != ['-u', os.environ.get('FAKE_ID_USER')]:\n"
            "    raise SystemExit(2)\n"
            "print(os.environ['FAKE_ID_UID'])\n"
        )
        id_command.chmod(0o755)

    def render(self, user="portal_test", group="morde_portal_test", fixture=None, output=None):
        environment = os.environ.copy()
        environment["PATH"] = str(self.bin_dir) + os.pathsep + environment.get("PATH", "")
        environment.update(self.fixtures if fixture is None else fixture)
        return subprocess.run(
            ["bash", str(SCRIPT), user, group, str(output or self.output)],
            capture_output=True,
            text=True,
            env=environment,
        )

    def test_renders_all_templates_with_one_group_without_system_changes(self):
        result = self.render()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({path.name for path in self.output.iterdir()}, EXPECTED)
        daemon = json.loads((self.output / "dsh-docker-daemon.json").read_text())
        self.assertEqual(daemon["group"], "morde_portal_test")
        guardian = (self.output / "morde-dsh-guardian.service").read_text()
        self.assertIn("Group=morde_portal_test", guardian)
        self.assertIn("--socket-mode=0660", guardian)
        for path in self.output.iterdir():
            self.assertNotIn("@PORTAL_GROUP@", path.read_text())

    def test_accepts_primary_group_membership_with_empty_member_list(self):
        fixture = {
            **self.fixtures,
            "FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:\nnogroup:x:65534:nobody",
        }

        result = self.render(fixture=fixture)

        self.assertEqual(result.returncode, 0, result.stderr)

    def test_accepts_membership_listed_as_supplementary_group(self):
        fixture = {
            **self.fixtures,
            "FAKE_PASSWD_ENTRIES": (
                "portal_test:x:1001:2002::/nonexistent:/usr/sbin/nologin\n"
                "nobody:x:65534:65534::/nonexistent:/usr/sbin/nologin"
            ),
            "FAKE_GROUP_ENTRIES": (
                "morde_portal_test:x:2001:portal_test\n"
                "portal_primary:x:2002:portal_test\n"
                "nogroup:x:65534:nobody"
            ),
        }

        result = self.render(fixture=fixture)

        self.assertEqual(result.returncode, 0, result.stderr)

    def test_rejects_invalid_or_non_dedicated_group_names(self):
        for group in ("@PORTAL_GROUP@", "BadGroup", "bad.group", "root", "docker", "a" * 33):
            with self.subTest(group=group):
                result = self.render(group=group)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list(self.output.iterdir()), [])

    def test_requires_existing_non_root_local_user_and_group_membership(self):
        cases = (
            ("missing local user", {"FAKE_PASSWD_ENTRIES": "nobody:x:65534:65534::/nonexistent:/usr/sbin/nologin"}),
            (
                "root user",
                {
                    "FAKE_PASSWD_ENTRIES": "portal_test:x:0:2001::/root:/bin/sh\nnobody:x:65534:65534::/nonexistent:/usr/sbin/nologin",
                    "FAKE_ID_UID": "0",
                },
            ),
            ("missing local group", {"FAKE_GROUP_ENTRIES": "nogroup:x:65534:nobody"}),
            (
                "another listed group member",
                {"FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:portal_test,other\nnogroup:x:65534:nobody"},
            ),
            (
                "user not in group",
                {
                    "FAKE_PASSWD_ENTRIES": "portal_test:x:1001:2999::/nonexistent:/usr/sbin/nologin\nnobody:x:65534:65534::/nonexistent:/usr/sbin/nologin",
                    "FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:\nnogroup:x:65534:nobody",
                },
            ),
            (
                "shared primary GID",
                {
                    "FAKE_PASSWD_ENTRIES": "portal_test:x:1001:2001::/nonexistent:/usr/sbin/nologin\nworker:x:1002:2001::/nonexistent:/usr/sbin/nologin\nnobody:x:65534:65534::/nonexistent:/usr/sbin/nologin",
                    "FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:portal_test\nnogroup:x:65534:nobody",
                },
            ),
            (
                "group alias with same GID",
                {"FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:portal_test\nportal-alias:x:2001:worker\nnogroup:x:65534:nobody"},
            ),
            (
                "duplicate group name",
                {"FAKE_GROUP_ENTRIES": "morde_portal_test:x:2001:portal_test\nmorde_portal_test:x:2002:other\nnogroup:x:65534:nobody"},
            ),
            ("id UID does not match local passwd entry", {"FAKE_ID_UID": "1002"}),
        )
        for name, overrides in cases:
            with self.subTest(name=name):
                fixture = {**self.fixtures, **overrides}
                result = self.render(fixture=fixture)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list(self.output.iterdir()), [])

    def test_rejects_invalid_portal_user_name(self):
        result = self.render(user="bad.user")

        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(self.output.iterdir()), [])

    def test_refuses_to_overwrite_any_rendered_file(self):
        existing = self.output / "morde-dsh-guardian.service"
        existing.write_text("keep me\n")

        result = self.render()

        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(existing.read_text(), "keep me\n")
        self.assertEqual({path.name for path in self.output.iterdir()}, {existing.name})

    def test_requires_an_existing_real_output_directory(self):
        result = subprocess.run(
            ["bash", str(SCRIPT), "portal_test", "morde_portal_test", str(self.output / "missing")],
            capture_output=True,
            text=True,
            env={
                **os.environ,
                "PATH": str(self.bin_dir) + os.pathsep + os.environ.get("PATH", ""),
                **self.fixtures,
            },
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.output / "missing").exists())


if __name__ == "__main__":
    unittest.main()
