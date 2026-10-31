#!/usr/bin/env python3
"""Local assertions for deployment parsing, binary architecture, and boot order."""

import importlib.util
import io
import json
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

    def test_admin_bootstrap_has_no_credential_file_or_cli_user_creation(self):
        for name in (".env.example", "portal.yaml", "install-portal-service.sh", "read-env-values.py"):
            with self.subTest(name=name):
                self.assertNotIn("CH_ADMIN_CREDENTIALS_FILE", (ROOT / name).read_text())
        with self.assertRaisesRegex(ValueError, "approved non-secret keys"):
            reader.parse_values(io.StringIO("CH_ADMIN_CREDENTIALS_FILE=/etc/cloudhost/admin.yaml\n"), ["CH_ADMIN_CREDENTIALS_FILE"])

        instructions = (ROOT / "README.md").read_text()
        self.assertIn("默认密码为 `admin`", instructions)
        self.assertIn("受信任的 SSH 本地隧道", instructions)
        self.assertIn("初始状态为待审核", instructions)
        self.assertIn("`portal user add` 入口均已移除", instructions)

    def test_preload_is_required_before_docker_can_start(self):
        preload = (ROOT / "morde-network-preload.service").read_text()
        dropin = (ROOT / "docker-network-preload.conf").read_text()
        post = (ROOT / "morde-network-policy.service").read_text()
        nftables = (ROOT / "nftables-workspace-sync.conf").read_text()
        self.assertIn("Before=docker.service", preload)
        self.assertIn("Requires=nftables.service", preload)
        self.assertIn("After=nftables.service", preload)
        self.assertIn("PartOf=docker.service nftables.service", preload)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh preload", preload)
        self.assertNotIn("ExecReload=", preload)
        self.assertIn("Requires=morde-network-preload.service", dropin)
        self.assertIn("After=morde-network-preload.service", dropin)
        self.assertIn("Requires=docker.service morde-network-preload.service", post)
        self.assertIn("PartOf=docker.service morde-network-preload.service nftables.service", post)
        self.assertIn("After=docker.service morde-network-preload.service", post)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh apply", post)
        self.assertNotIn("ExecReload=", post)
        self.assertIn("Wants=morde-portal.service", nftables)
        self.assertIn("ExecStartPre=/opt/cloudhost/network-policy.sh nftables-before", nftables)
        self.assertIn("ExecStartPost=/opt/cloudhost/network-policy.sh preload", nftables)
        self.assertIn("ExecStop=\nExecStop=/opt/cloudhost/network-policy.sh nftables-stop", nftables)
        self.assertIn("ExecReload=\nExecReload=/opt/cloudhost/network-policy.sh nftables-reload", nftables)

    def test_docker_restart_restarts_post_policy_then_portal(self):
        # PartOf= propagates restarts; After= orders policy before Portal recovery.
        post = (ROOT / "morde-network-policy.service").read_text()
        installer = (ROOT / "install-portal-service.sh").read_text()
        self.assertIn("Requires=docker.service morde-network-preload.service", post)
        self.assertIn("PartOf=docker.service morde-network-preload.service nftables.service", post)
        self.assertIn("After=docker.service morde-network-preload.service", post)
        self.assertIn("ExecStart=/opt/cloudhost/network-policy.sh apply", post)
        self.assertIn("PartOf=morde-network-policy.service", installer)
        self.assertIn("PartOf=morde-network-policy.service nftables.service", installer)
        self.assertIn("Requires=docker.service morde-network-policy.service", installer)
        self.assertIn("After=network-online.target docker.service morde-network-policy.service nftables.service", installer)
        self.assertIn("mountpoint -q -- \"$path\"", installer)
        self.assertIn('sync_mount_binding="BindsTo=$sync_mount_units_text"', installer)
        self.assertIn('local -a roots=("${CH_WORKSPACE_SYNC_CURRENT_FILES_DIR:-}")', installer)
        self.assertIn("workspace_sync_current_root_from_config()", installer)
        self.assertIn("check_workspace_sync_current_root_config", installer)
        self.assertIn('[[ $configured_root == "$CH_WORKSPACE_SYNC_CURRENT_FILES_DIR" ]]', installer)
        self.assertIn('for key in CH_WORKSPACE_STORAGE_ROOT_DIR CH_SITE_STORAGE_ROOT_DIR CH_WORKSPACE_SYNC_CURRENT_FILES_DIR; do', installer)
        self.assertIn('CH_DOCKER_DATA_DIR CH_POSTGRES_DATA_DIR', installer)
        self.assertIn("systemd-escape --path --suffix=mount", installer)
        self.assertIn('systemctl is-active --quiet "$mount_unit"', installer)
        self.assertIn("--check-workspace-sync-mount-bindings", installer)
        self.assertIn('systemctl show --property=BindsTo --value morde-portal.service', installer)
        self.assertIn("ExecStartPre=/bin/bash $repo_root/deploy/single/install-portal-service.sh --check-workspace-sync-mount-bindings", installer)
        self.assertIn("ExecStartPre=/opt/cloudhost/network-policy.sh check", installer)

    def test_preload_does_not_contact_docker_and_shares_post_boot_rules(self):
        policy = (ROOT / "network-policy.sh").read_text()
        preload = policy.split("preload_network() {", 1)[1].split("\n}", 1)[0]
        apply = policy.split("apply_network() {", 1)[1].split("\n}", 1)[0]
        self.assertNotRegex(preload, r"(?m)^\s*docker\b")
        self.assertLess(preload.index("config_preflight preload"), preload.index("install_policy_rules"))
        self.assertLess(apply.index("config_preflight apply"), apply.index("install_policy_rules"))
        self.assertEqual(policy.count('cat >"$TMP_DIR/rules.nft"'), 1)

    def test_workspace_sync_egress_is_limited_to_the_static_relay(self):
        policy = (ROOT / "network-policy.sh").read_text()
        self.assertIn('public_value == "127.0.0.1:22067"', policy)
        self.assertIn("public.version != 4 or not public.is_global", policy)
        self.assertIn("bind.is_unspecified or bind.is_loopback or bind.is_link_local or bind.is_multicast", policy)
        self.assertIn("add set inet cloudhost relay_enabled_ifaces { type ifname; }", policy)
        self.assertIn('iifname "msync*" iifname @relay_enabled_ifaces meta l4proto tcp', policy)
        self.assertIn('oifname "msync*" oifname @relay_enabled_ifaces', policy)
        self.assertIn('ct state {{ established, related }} ct original ip daddr {ip} ct original proto-dst 22067 accept', policy)
        self.assertIn("lock_policy_state no", policy)
        self.assertIn('iifname "msync*" counter drop', policy)
        self.assertIn('oifname "msync*" counter drop', policy)
        self.assertIn('relay-disable-stdin', policy)
        self.assertIn('relay-enable-stdin', policy)
        self.assertIn('network.get("Labels") != expected_labels', policy)
        self.assertIn('iifname "msync*" meta l4proto tcp ct state established ct original proto-dst 8384 accept', policy)
        self.assertIn("CONFIG_FINGERPRINT_PATH", policy)
        self.assertIn("workspace_sync_config_fingerprint", policy)
        self.assertIn("relay-disable-all", policy)
        self.assertIn("configured Workspace relay address differs from the root-applied nftables policy", policy)
        self.assertLess(policy.index("workspace_sync_relay_bind_ipv4()"), policy.index("build_policy_rules()"))

    def test_nftables_lifecycle_preserves_deny_policy_and_stops_before_reload(self):
        policy = (ROOT / "network-policy.sh").read_text()
        installer = (ROOT / "install-portal-service.sh").read_text()
        stop = policy.split("nftables_stop() {", 1)[1].split("\n}", 1)[0]
        reload = policy.split("nftables_reload() {", 1)[1].split("\n}", 1)[0]
        before = policy.split("nftables_before() {", 1)[1].split("\n}", 1)[0]
        self.assertLess(stop.index("stop_portal_service"), stop.index("clear_workspace_sync_relay_gates"))
        self.assertLess(stop.index("stop_workspace_sync_nodes"), stop.index("clear_workspace_sync_relay_gates"))
        self.assertNotIn('"$NFT_BIN" flush ruleset', stop)
        self.assertIn("Morde deny policy was preserved", stop)
        self.assertLess(before.index("portal_service_stopped"), before.index("stop_workspace_sync_nodes"))
        self.assertLess(reload.index("stop_portal_service"), reload.index("stop_workspace_sync_nodes"))
        self.assertLess(reload.index("stop_workspace_sync_nodes"), reload.index('"$NFT_BIN" --check --file "$combined"'))
        self.assertLess(reload.index('"$NFT_BIN" --check --file "$combined"'), reload.index('"$NFT_BIN" --file "$combined"'))
        self.assertLess(reload.index('"$NFT_BIN" --file "$combined"'), reload.index("systemctl start --no-block morde-portal.service"))
        self.assertIn("ExecStop=\nExecStop=/opt/cloudhost/network-policy.sh nftables-stop", (ROOT / "nftables-workspace-sync.conf").read_text())
        self.assertIn("ExecReload=\nExecReload=/opt/cloudhost/network-policy.sh nftables-reload", (ROOT / "nftables-workspace-sync.conf").read_text())
        self.assertIn("validate_nftables_base_unit", installer)
        self.assertIn("ExecStop=/usr/sbin/nft flush ruleset", installer)
        self.assertIn("ExecStartPre|ExecStartPost|ExecStopPost", installer)
        self.assertIn("systemctl reload-or-restart nftables.service", installer)
        self.assertIn("systemctl stop morde-portal.service", installer)

    def test_installer_installs_and_starts_in_dependency_order(self):
        installer = (ROOT / "install-portal-service.sh").read_text()
        self.assertNotIn('. "$env_file"', installer)
        self.assertIn("read-env-values.py", installer)
        self.assertIn("[[ $CH_DOCKER_HOST == unix:///* && -S $docker_socket ]] || die 'CH_DOCKER_HOST must point to a local Unix Docker socket'", installer)
        self.assertIn("docker.service.d/50-morde-network-preload.conf", installer.replace("$docker_dropin_dir", "docker.service.d"))
        self.assertRegex(installer, r"systemctl enable morde-network-preload.service morde-network-policy.service morde-portal.service\s+systemctl stop morde-portal.service\s+systemctl reload-or-restart nftables.service\s+systemctl reload-or-restart morde-network-preload.service\s+systemctl reload-or-restart morde-network-policy.service\s+systemctl restart morde-portal.service")
        self.assertNotIn("systemctl restart morde-network-preload.service", installer)
        self.assertLess(installer.index("systemctl daemon-reload"), installer.index("systemctl reload-or-restart morde-network-preload.service"))
        self.assertIn("validate-portal-binary.py", installer)
        self.assertIn("MORDE_SYNC_RELAY_GATE", installer)
        self.assertIn("relay-enable-stdin, /opt/cloudhost/network-policy.sh relay-disable-stdin, /opt/cloudhost/network-policy.sh relay-disable-all", installer)
        self.assertIn("quota_socket_root=/var/lib/morde-quota", installer)
        self.assertIn("SupplementaryGroups=65532", installer)
        self.assertIn("visudo -cf", installer)
        self.assertLess(installer.index("CH_DOCKER_HOST must point to a local Unix Docker socket"), installer.index('install -d -o root -g root -m 0755 "$docker_dropin_dir"'))
        self.assertLess(installer.index("validate-portal-binary.py"), installer.index('install -d -o root -g root -m 0755 "$docker_dropin_dir"'))
        for script in ("network-policy.sh", "install-portal-service.sh"):
            result = subprocess.run(["bash", "-n", str(ROOT / script)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_installer_requires_user_admin_mobile_and_workbench_entries(self):
        installer = (ROOT / "install-portal-service.sh").read_text()
        for entry in (
            "$repo_root/web/portal/dist/index.html",
            "$repo_root/web/portal/dist/admin/index.html",
            "$repo_root/web/portal/dist/app/index.html",
            "$repo_root/web/workbench/dist/index.html",
        ):
            self.assertIn(entry, installer)

    def test_automated_portal_gate_builds_the_mobile_app(self):
        gates = (ROOT / "verify/05-automated.sh").read_text()
        package = json.loads((ROOT.parents[1] / "web/portal/package.json").read_text())
        self.assertIn("npm run build", gates)
        self.assertIn("project/nanoMuse/web run build", package["scripts"]["build"])

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
