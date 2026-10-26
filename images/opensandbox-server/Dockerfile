FROM docker.io/opensandbox/server:release-1.1.1@sha256:bcd5f264101262cebfa7e1ff9cd0424403e42f31a6596e4364cf90d8cb5c936b

# OpenSandbox 1.1.1 only publishes its AIO and execd ports; Morde Runtime needs direct TLS on 8090.
RUN python3 - <<'PY'
from pathlib import Path


def replace_exact(path: Path, old: str, new: str, count: int = 1) -> None:
    source = path.read_text()
    if source.count(old) != count:
        raise SystemExit(f"unexpected OpenSandbox source in {path}: {old!r}")
    path.write_text(source.replace(old, new))


docker_service = Path("/app/opensandbox_server/services/docker/docker_service.py")
replace_exact(
    docker_service,
    'exposed_ports: Optional[list[str]] = ["44772", "8080"]',
    'exposed_ports: Optional[list[str]] = ["44772", "8080", "8090"]',
)
replace_exact(
    docker_service,
    '                host_http_port = sidecar_port_bindings["8080"][1]\n',
    '                host_http_port = sidecar_port_bindings["8080"][1]\n'
    '                labels["morde.cloudhost/runtime-tls-port"] = str(sidecar_port_bindings["8090"][1])\n',
)
replace_exact(
    docker_service,
    '                    host_http_port = port_bindings["8080"][1]\n'
    '                    host_config_kwargs["port_bindings"] = normalize_port_bindings(port_bindings)\n',
    '                    host_http_port = port_bindings["8080"][1]\n'
    '                    labels["morde.cloudhost/runtime-tls-port"] = str(port_bindings["8090"][1])\n'
    '                    host_config_kwargs["port_bindings"] = normalize_port_bindings(port_bindings)\n',
)
replace_exact(
    docker_service,
    '                            host_http_port = port_bindings["8080"][1]\n'
    '                            host_config_kwargs["port_bindings"] = normalize_port_bindings(port_bindings)\n',
    '                            host_http_port = port_bindings["8080"][1]\n'
    '                            labels["morde.cloudhost/runtime-tls-port"] = str(port_bindings["8090"][1])\n'
    '                            host_config_kwargs["port_bindings"] = normalize_port_bindings(port_bindings)\n',
)

networking = Path("/app/opensandbox_server/services/docker/networking.py")
replace_exact(
    networking,
    '        if execd_host_port is None:\n',
    '        if port == 8090:\n'
    '            # The 44772 proxy is HTTP-only; Morde Runtime requires direct TLS.\n'
    '            runtime_host_port = self._parse_host_port_label(\n'
    '                labels.get("morde.cloudhost/runtime-tls-port"),\n'
    '                "morde.cloudhost/runtime-tls-port",\n'
    '            )\n'
    '            if runtime_host_port is None:\n'
    '                raise HTTPException(\n'
    '                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,\n'
    '                    detail={\n'
    '                        "code": SandboxErrorCodes.NETWORK_MODE_ENDPOINT_UNAVAILABLE,\n'
    '                        "message": "Missing host port mapping for container port 8090.",\n'
    '                    },\n'
    '                )\n'
    '            endpoint = Endpoint(endpoint=f"{public_host}:{runtime_host_port}")\n'
    '            if include_egress_auth_headers:\n'
    '                self._attach_egress_auth_headers(endpoint, labels, port)\n'
    '            return endpoint\n'
    '\n'
    '        if execd_host_port is None:\n',
)
PY
