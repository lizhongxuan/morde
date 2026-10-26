#!/usr/bin/env python3
"""Local assertions for deployment parsing, binary architecture, and boot order."""

import importlib.util
import io
import os
import pathlib
import socket
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("read_env_values", ROOT / "read-env-values.py")
assert spec and spec.loader
reader = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reader)
binary_spec = importlib.util.spec_from_file_location("validate_portal_binary", ROOT / "validate-portal-binary.py")
assert binary_spec and binary_spec.loader
binary_validator = importlib.util.module_from_spec(binary_spec)
binary_spec.loader.exec_module(binary_validator)


class DeploymentTests(unittest.TestCase):
    def test_environment_reader_never_executes_other_lines(self):
        source = io.StringIO(
            "CH_DATA_KEY_FILE='/etc/cloudhost/data.key'\n"
            "CH_CADDY_PORTAL_TLS='tls {\n  on_demand\n}'\n"
            "OTHER=$(touch /tmp/never-executed)\n"
            "CH_OSB_URL=http://127.0.0.1:18080\n"
        )
        self.assertEqual(
            reader.parse_values(source, ["CH_DATA_KEY_FILE", "CH_OSB_URL"]),
            ["/etc/cloudhost/data.key", "http://127.0.0.1:18080"],
        )

    def test_environment_reader_rejects_ambiguous_or_executable_values(self):
        for source in (
            "CH_DATA_KEY_FILE=$(touch /tmp/never-executed)\n",
            "CH_DATA_KEY_FILE=/a\nCH_DATA_KEY_FILE=/b\n",
            "CH_DATA_KEY_FILE = /a\n",
        ):
            with self.subTest(source=source), self.assertRaises(ValueError):
                reader.parse_values(io.StringIO(source), ["CH_DATA_KEY_FILE"])

    def test_preload_is_required_before_docker_can_start(self):
        preload = (ROOT / "morde-network-preload.service").read_text()
        dropin = (ROOT / "docker-network-preload.conf").read_text()
        post = (ROOT / "morde-network-policy.service").read_text()
        self.assertIn("Before=docker.service", preload)
        self.assertIn("PartOf=docker.service", preload)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh preload", preload)
        self.assertIn("ExecReload=/opt/cloudhost/network-policy.sh preload", preload)
        self.assertIn("Requires=morde-network-preload.service", dropin)
        self.assertIn("After=morde-network-preload.service", dropin)
        self.assertIn("Requires=docker.service morde-network-preload.service", post)
        self.assertIn("After=docker.service morde-network-preload.service", post)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh apply", post)
        self.assertIn("ExecReload=/opt/cloudhost/network-policy.sh apply", post)

    def test_docker_restart_restarts_post_policy_then_portal(self):
        # systemd Requires= propagates an explicit restart of the required unit;
        # After= orders the restarted units (systemd.unit(5)).
        post = (ROOT / "morde-network-policy.service").read_text()
        installer = (ROOT / "install-portal-service.sh").read_text()
        self.assertIn("Requires=docker.service morde-network-preload.service", post)
        self.assertIn("After=docker.service morde-network-preload.service", post)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh apply", post)
        self.assertIn("Requires=docker.service morde-network-policy.service", installer)
        self.assertIn("After=network-online.target docker.service morde-network-policy.service", installer)
        self.assertIn("ExecStartPre=/opt/cloudhost/network-policy.sh check", installer)

    def test_preload_does_not_contact_docker_and_shares_post_boot_rules(self):
        policy = (ROOT / "network-policy.sh").read_text()
        preload = policy.split("preload_network() {", 1)[1].split("\n}", 1)[0]
        apply = policy.split("apply_network() {", 1)[1].split("\n}", 1)[0]
        self.assertNotRegex(preload, r"(?m)^\s*docker\b")
        self.assertLess(preload.index("config_preflight preload"), preload.index("install_policy_rules"))
        self.assertLess(apply.index("config_preflight apply"), apply.index("install_policy_rules"))
        self.assertEqual(policy.count('cat >"$TMP_DIR/rules.nft"'), 1)

    def test_installer_installs_and_starts_in_dependency_order(self):
        installer = (ROOT / "install-portal-service.sh").read_text()
        self.assertNotIn('. "$env_file"', installer)
        self.assertIn("read-env-values.py", installer)
        self.assertIn("[[ $CH_DOCKER_HOST == unix:///* && -S $docker_socket ]] || die 'CH_DOCKER_HOST must point to a local Unix Docker socket'", installer)
        self.assertIn("docker.service.d/50-morde-network-preload.conf", installer.replace("$docker_dropin_dir", "docker.service.d"))
        self.assertRegex(installer, r"systemctl reload-or-restart morde-network-preload.service\s+systemctl reload-or-restart morde-network-policy.service\s+systemctl restart morde-portal.service")
        self.assertNotIn("systemctl restart morde-network-preload.service", installer)
        self.assertLess(installer.index("systemctl daemon-reload"), installer.index("systemctl reload-or-restart morde-network-preload.service"))
        self.assertIn("validate-portal-binary.py", installer)
        self.assertLess(installer.index("CH_DOCKER_HOST must point to a local Unix Docker socket"), installer.index('install -d -o root -g root -m 0755 "$docker_dropin_dir"'))
        self.assertLess(installer.index("validate-portal-binary.py"), installer.index('install -d -o root -g root -m 0755 "$docker_dropin_dir"'))
        for script in ("network-policy.sh", "install-portal-service.sh"):
            result = subprocess.run(["bash", "-n", str(ROOT / script)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_installer_requires_user_admin_and_workbench_entries(self):
        installer = (ROOT / "install-portal-service.sh").read_text()
        for entry in (
            "$repo_root/web/portal/dist/index.html",
            "$repo_root/web/portal/dist/admin/index.html",
            "$repo_root/web/workbench/dist/index.html",
        ):
            self.assertIn(entry, installer)
        self.assertNotIn("web/portal/dist/app/index.html", installer)

    def test_mvp_automated_gates_exclude_mobile_workspace(self):
        gates = (ROOT / "verify/05-automated.sh").read_text()
        self.assertNotIn("project/nanoMuse", gates)
        self.assertIn("portal lint, typecheck, unit tests, and production build", gates)

    def test_installer_accepts_only_existing_local_docker_socket(self):
        installer = (ROOT / "install-portal-service.sh").read_text()
        start = installer.index("docker_socket=${CH_DOCKER_HOST#unix://}")
        end = installer.index("\n\nfor key in", start)
        guard = "die() { exit 1; }\n" + installer[start:end]
        with tempfile.TemporaryDirectory() as directory:
            socket_path = pathlib.Path(directory) / "docker.sock"
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as docker_socket:
                docker_socket.bind(str(socket_path))
                local = subprocess.run(
                    ["bash", "-c", guard],
                    env={**os.environ, "CH_DOCKER_HOST": f"unix://{socket_path}"},
                )
                self.assertEqual(local.returncode, 0)

            for endpoint in ("tcp://127.0.0.1:2375", "ssh://docker.example"):
                with self.subTest(endpoint=endpoint):
                    remote = subprocess.run(
                        ["bash", "-c", guard],
                        env={**os.environ, "CH_DOCKER_HOST": endpoint},
                    )
                    self.assertNotEqual(remote.returncode, 0)

    def test_portal_binary_must_match_linux_target_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = pathlib.Path(directory) / "portal"
            for architecture, machine in (("x86_64", 62), ("aarch64", 183)):
                header = bytearray(20)
                header[:6] = b"\x7fELF\x02\x01"
                header[18:20] = machine.to_bytes(2, "little")
                binary.write_bytes(header)
                binary_validator.validate_elf(binary, architecture)

            header[18:20] = (62).to_bytes(2, "little")
            binary.write_bytes(header)
            with self.assertRaisesRegex(ValueError, "does not match aarch64"):
                binary_validator.validate_elf(binary, "aarch64")

    def test_portal_binary_rejects_macho_truncated_and_unknown_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = pathlib.Path(directory) / "portal"
            for content, architecture, message in (
                (b"\xcf\xfa\xed\xfe" + bytes(20), "x86_64", "ELF64"),
                (b"\x7fELF", "x86_64", "ELF64"),
                (b"\x7fELF" + bytes(16), "riscv64", "unsupported Linux architecture"),
            ):
                with self.subTest(architecture=architecture, message=message):
                    binary.write_bytes(content)
                    with self.assertRaisesRegex(ValueError, message):
                        binary_validator.validate_elf(binary, architecture)


if __name__ == "__main__":
    unittest.main()
