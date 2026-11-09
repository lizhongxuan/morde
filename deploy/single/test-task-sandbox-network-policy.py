from pathlib import Path
import re
import tomllib
import unittest


ROOT = Path(__file__).resolve().parent
CONFIG_PATH = ROOT / "opensandbox-task.toml"
SCRIPT_PATH = ROOT / "task-sandbox-network-policy.sh"
RESOLVER_PATH = ROOT / "task-sandbox-resolv.conf"


class TaskSandboxNetworkPolicyContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.config_text = CONFIG_PATH.read_text(encoding="utf-8")
        cls.config = tomllib.loads(cls.config_text)
        cls.script = SCRIPT_PATH.read_text(encoding="utf-8")
        cls.resolver = RESOLVER_PATH.read_text(encoding="ascii")

    def test_task_opensandbox_is_separate_loopback_runsc_profile(self):
        self.assertEqual(self.config["server"], {"host": "127.0.0.1", "port": 18081, "eip": "127.0.0.1"})
        self.assertEqual(self.config["runtime"]["type"], "docker")
        self.assertTrue(self.config["runtime"]["execd_image"].endswith("release-1.1.1@sha256:cda2bc35409bbf57561599b316cafb899598f7dce671332e7ceda14eb1cfeded"))
        self.assertEqual(self.config["secure_runtime"], {"type": "gvisor", "docker_runtime": "runsc"})

    def test_dedicated_bridge_and_execd_host_port_range_are_fixed(self):
        docker = self.config["docker"]
        self.assertEqual(docker["network_mode"], "morde-task-sandbox")
        self.assertEqual(docker["publish_host"], "127.0.0.1")
        self.assertEqual((docker["port_range_min"], docker["port_range_max"]), (46000, 46999))
        self.assertEqual(docker["pids_limit"], 64)
        self.assertEqual(docker["no_new_privileges"], True)
        self.assertEqual(docker["drop_capabilities"], ["NET_RAW"])
        self.assertEqual(docker["sandbox_binds"], ["/etc/cloudhost/task-sandbox-resolv.conf:/etc/resolv.conf:ro"])
        self.assertEqual(docker["sandbox_env"], {})
        self.assertNotIn("egress", self.config)
        self.assertNotIn("networkPolicy", self.config)

    def test_task_resolver_has_no_upstream_dns_server(self):
        self.assertEqual(self.resolver.splitlines(), ["nameserver 127.0.0.1", "options attempts:1 timeout:1"])
        self.assertIn('protect_file "$RESOLVER_PATH"', self.script)
        self.assertIn('resolver != ["nameserver 127.0.0.1", "options attempts:1 timeout:1"]', self.script)
        self.assertIn('"FAIL task-sandbox-network-policy task resolver must have no upstream DNS server"', self.script)
        self.assertIn("not prove that direct queries", self.config_text)
        self.assertIn("Docker's embedded 127.0.0.11 resolver", self.config_text)
        self.assertIn("systemd preload ordering", self.config_text)

    def test_live_network_checks_all_required_isolation_options(self):
        for expected in (
            '"Name") != "morde-task-sandbox"',
            '"Driver") != "bridge"',
            '"EnableIPv6") is not False',
            '"com.docker.network.bridge.name": "mtbr0"',
            '"com.docker.network.bridge.enable_icc": "false"',
            '"com.docker.network.bridge.enable_ip_masquerade": "false"',
            '"com.docker.network.bridge.host_binding_ipv4": "127.0.0.1"',
            'str(subnet) != "172.31.0.0/24"',
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, self.script)

    def test_host_input_and_all_forwarding_are_denied_except_exact_execd_reply(self):
        self.assertIn('input iifname "$BRIDGE_NAME" counter drop', self.script)
        self.assertIn('forward iifname "$BRIDGE_NAME" counter drop', self.script)
        self.assertIn('forward oifname "$BRIDGE_NAME" counter drop', self.script)
        self.assertIn('ct direction reply', self.script)
        self.assertIn('ct original ip saddr 127.0.0.1', self.script)
        self.assertIn('ct original ip daddr 127.0.0.1', self.script)
        self.assertIn('ct original proto-dst $EXEC_PORT_RANGE_MIN-$EXEC_PORT_RANGE_MAX', self.script)
        self.assertIn('tcp sport $EXECD_PORT counter accept', self.script)

    def test_only_loopback_dnat_to_execd_is_allowed_from_host(self):
        self.assertIn('output oifname "$BRIDGE_NAME" ct status dnat', self.script)
        self.assertIn('ct state new ct direction original', self.script)
        self.assertIn('ip daddr $SUBNET tcp dport $EXECD_PORT counter accept', self.script)
        self.assertIn('output oifname "$BRIDGE_NAME" counter drop', self.script)
        self.assertEqual(len(re.findall(r'EXECD_PORT=\d+', self.script)), 1)
        self.assertIn('readonly EXECD_PORT=44772', self.script)
        self.assertIn('"userland-proxy") is not False', self.script)

    def test_policy_changes_only_own_tables_with_check_then_atomic_batch(self):
        self.assertIn('destroy table inet morde_task_sandbox', self.script)
        self.assertIn('destroy table bridge morde_task_sandbox', self.script)
        self.assertNotRegex(self.script, r"(?m)^\s*(?:flush|destroy|delete)\s+(?:ruleset|table\s+(?:inet|bridge)\s+cloudhost)")
        check = self.script.split("check() {", 1)[1].split("\n}\n", 1)[0]
        self.assertNotIn("--file", check)
        self.assertNotIn("install ", check)
        self.assertNotIn("rm -f", check)
        self.assertIn('"$NFT_BIN" --check --file "$TMP_DIR/rules.nft"', self.script)
        self.assertIn('"$NFT_BIN" --file "$TMP_DIR/rules.nft"', self.script)

    def test_dynamic_nft_counters_are_excluded_from_policy_fingerprint(self):
        fingerprint = self.script.split("table_fingerprint() {", 1)[1].split("\n}\n", 1)[0]
        self.assertIn('if key == "counter" and isinstance(val, dict):', fingerprint)
        self.assertIn("normalized[key] = {}", fingerprint)
        self.assertIn('if key in {"handle", "metainfo"}:', fingerprint)

    def test_preload_refuses_to_change_policy_while_task_service_or_sandboxes_exist(self):
        self.assertIn("ensure_task_sandbox_idle", self.script)
        idle = self.script.split("ensure_task_sandbox_idle() {", 1)[1].split("\n}\n", 1)[0]
        self.assertIn('ps -aq --filter "network=$NETWORK_NAME"', idle)
        self.assertIn("preload requires the task OpenSandbox service stopped", idle)
        self.assertIn("task OpenSandbox must use its protected read-only config", self.script)

    def test_preload_serializes_idle_check_and_rechecks_before_ready_state(self):
        preload = self.script.split("preload_policy() {", 1)[1].split("\n}\n", 1)[0]
        first_idle = preload.index("ensure_task_sandbox_idle")
        apply = preload.index("apply_policy")
        second_idle = preload.rindex("ensure_task_sandbox_idle")
        ready_state = preload.index('write_state_file config-fingerprint')
        self.assertLess(first_idle, apply)
        self.assertLess(apply, second_idle)
        self.assertLess(second_idle, ready_state)
        for caller in ("preload() {", "prepare_network() {"):
            body = self.script.split(caller, 1)[1].split("\n}\n", 1)[0]
            self.assertLess(body.index("lock_policy"), body.index("preload_policy"))

    def test_apply_serializes_live_preflight_and_revalidates_after_policy_install(self):
        apply = self.script.split("apply() {", 1)[1].split("\n}\n", 1)[0]
        lock = apply.index("lock_policy")
        live_checks = [
            apply.index("live_docker_runtime_preflight"),
            apply.index("live_network_preflight"),
            apply.index("live_task_server_preflight"),
        ]
        install = apply.index("apply_policy")
        final_live_checks = apply.rindex("live_docker_runtime_preflight")
        state = apply.index('write_state_file config-fingerprint')
        self.assertTrue(all(lock < check < install for check in live_checks))
        self.assertLess(install, final_live_checks)
        self.assertLess(final_live_checks, state)


if __name__ == "__main__":
    unittest.main()
