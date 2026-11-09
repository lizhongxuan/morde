from pathlib import Path
import tomllib
import unittest


ROOT = Path(__file__).resolve().parent


class TaskSandboxDeploymentContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
        cls.unit = (ROOT / "morde-task-sandbox.service.in").read_text(encoding="utf-8")
        cls.installer = (ROOT / "install-task-sandbox-service.sh").read_text(encoding="utf-8")
        cls.policy = (ROOT / "task-sandbox-network-policy.sh").read_text(encoding="utf-8")
        cls.host_policy = (ROOT / "network-policy.sh").read_text(encoding="utf-8")
        cls.task_config = tomllib.loads((ROOT / "opensandbox-task.toml").read_text(encoding="utf-8"))

    def test_task_opensandbox_is_opt_in_and_never_autostarts_under_docker(self):
        service = self.compose.split("  opensandbox-task-server:\n", 1)[1].split("\n  caddy:", 1)[0]
        self.assertIn("profiles: [task-sandbox]", service)
        self.assertIn('restart: "no"', service)
        self.assertIn('network_mode: host', service)
        self.assertIn('/etc/opensandbox/opensandbox-task.toml:ro', service)
        self.assertEqual(self.task_config["server"]["host"], "127.0.0.1")
        self.assertEqual(self.task_config["server"]["port"], 18081)

    def test_systemd_orders_network_and_preload_before_server_then_verifies_policy(self):
        self.assertIn("Requires=docker.service morde-network-policy.service", self.unit)
        self.assertIn("After=network-online.target docker.service morde-network-policy.service", self.unit)
        prepare = self.unit.index("ExecStartPre=/opt/cloudhost/task-sandbox-network-policy.sh prepare-network")
        start = self.unit.index("ExecStart=@DOCKER_BIN@ compose")
        verify = self.unit.index("ExecStartPost=/opt/cloudhost/task-sandbox-network-policy.sh apply")
        self.assertLess(prepare, start)
        self.assertLess(start, verify)
        self.assertIn("ExecStop=@DOCKER_BIN@ compose", self.unit)
        self.assertIn("--force --stop opensandbox-task-server", self.unit)
        self.assertIn("ReadWritePaths=/etc/cloudhost/task-sandbox-network-policy", self.unit)
        self.assertNotIn("\n[Install]\n", self.unit)

    def test_installer_is_local_and_never_enables_or_starts_the_unit(self):
        self.assertIn("uname -s", self.installer)
        self.assertIn("[[ $EUID == 0 ]]", self.installer)
        self.assertIn("systemctl daemon-reload", self.installer)
        self.assertNotIn("systemctl enable", self.installer)
        self.assertNotIn("systemctl start", self.installer)
        self.assertNotIn("systemctl restart", self.installer)
        self.assertIn("cloudhost.env must have mode 0600", self.installer)
        self.assertIn("no service or network policy was started", self.installer)

    def test_prepared_bridge_uses_exact_restricted_options_and_revalidates(self):
        self.assertIn("ensure_network() {", self.policy)
        self.assertIn("--driver bridge", self.policy)
        self.assertIn('--subnet "$SUBNET"', self.policy)
        self.assertIn('--gateway "$GATEWAY"', self.policy)
        self.assertIn('com.docker.network.bridge.enable_icc=false', self.policy)
        self.assertIn('com.docker.network.bridge.enable_ip_masquerade=false', self.policy)
        self.assertIn('com.docker.network.bridge.host_binding_ipv4=127.0.0.1', self.policy)
        prepare = self.policy.split("prepare_network() {", 1)[1].split("\n}", 1)[0]
        self.assertLess(prepare.index("ensure_task_sandbox_idle"), prepare.index("ensure_network"))
        self.assertLess(prepare.index("ensure_network"), prepare.index("preload_policy"))
        self.assertIn("Keep the policy lock from bridge creation", self.policy)
        self.assertIn("    prepare-network)", self.policy)

    def test_global_nftables_lifecycle_refuses_to_remove_policy_while_task_containers_exist(self):
        self.assertIn('"$task_policy" assert-idle', self.host_policy)
        for name in ("nftables_before", "nftables_stop", "nftables_reload"):
            body = self.host_policy.split(f"{name}() {{", 1)[1].split("\n}", 1)[0]
            self.assertIn("task_sandbox_idle_preflight", body)
        self.assertIn('assert-idle)', self.policy)
        self.assertIn('assert-idle-locked)', self.policy)
        self.assertIn("task bridge", self.policy)
        assert_idle = self.policy.split("assert_idle() {", 1)[1].split("\n}", 1)[0]
        assert_idle_locked = self.policy.split("assert_idle_locked() {", 1)[1].split("\n}", 1)[0]
        self.assertIn("lock_policy", assert_idle)
        self.assertIn("systemctl show --property=ActiveState --value docker.service", assert_idle_locked)
        self.assertIn("systemctl show --property=ActiveState --value morde-task-sandbox.service", assert_idle_locked)
        self.assertIn("inactive|failed)", assert_idle_locked)
        self.assertIn("Docker service is transitioning", assert_idle_locked)
        preflight = self.host_policy.split("task_sandbox_idle_preflight() {", 1)[1].split("\n}", 1)[0]
        self.assertIn('exec 8>"$lock_path"', preflight)
        self.assertIn('flock -x 8', preflight)
        self.assertIn('exec 9>"$LOCK_PATH"', self.host_policy)
        self.assertIn('flock -x 9', self.host_policy)
        self.assertIn('assert-idle-locked', self.host_policy)
        for name in ("nftables_before", "nftables_stop", "nftables_reload"):
            body = self.host_policy.split(f"{name}() {{", 1)[1].split("\n}", 1)[0]
            self.assertLess(body.index("task_sandbox_idle_preflight"), body.index("lock_policy_state"))


if __name__ == "__main__":
    unittest.main()
