# Portal API assembly

`api.New` accepts the local service ports and portal file system used by the handlers. `cmd/portal` wires templates, hosts, workspaces, API keys, admin operations, readiness, MCP, and static assets.

Tests may omit optional ports to verify the documented sanitized `503` response. Production routes do not use placeholder handlers.
