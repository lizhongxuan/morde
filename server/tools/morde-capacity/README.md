# Morde conversation capacity runner

This local CLI simulates online conversation clients by periodically reading conversation history and active Tasks. It can also hold concurrent Task event SSE streams, reconnect them using their last event cursor, and reports request latency/status, active streams, errors, and client memory. Optional message and approval scenarios submit one write per selected virtual user.

Before reading the credential manifest or opening load connections, it checks `/api/capabilities` and exits unless the target advertises `cloud_tasks`. After validating the private manifest, it probes conversation history and the active-task endpoint. If the conversation-scoped task route returns 404, it uses the account task list only when that endpoint returns 200, and records the selected `task_poll_scope`; other failures stop before load begins.

The scenario manifest contains credentials and must be a local regular file with mode `0600`. Do not commit or share it. The runner never prints credentials, message text, or Task IDs. It accepts at most 10,000 virtual clients. Non-loopback targets require HTTPS and an exact `-allow-host` match.

Example manifest shape:

```json
{
  "users": [
    {
      "access_token": "chc_REDACTED",
      "actor_id": "u0000001",
      "service_instance_id": "00000000-0000-4000-8000-000000000001",
      "conversation_id": "c0000001",
      "task_id": "q0000001",
      "long_task": true,
      "approval": {
        "task_id": "q0000002",
        "approval_id": "approval-02",
        "version": 1
      }
    }
  ]
}
```

Run from `server/`:

```sh
go run ./tools/morde-capacity \
  -base-url https://staging.example.test \
  -environment staging \
  -success-criteria '10,000 virtual clients; history poll p95 <= 1s; no unexpected HTTP errors' \
  -allow-host staging.example.test \
  -manifest /path/to/private-scenario.json \
  -connections 10000 -task-streams 1000 -long-task-streams 1000 \
  -duration 5m -ramp 1m -poll-interval 30s -reconnect 1m
```

The manifest may contain one or more profiles; profiles cycle to fill `-connections`, allowing read-only load to reuse a test account without provisioning thousands of accounts. Reusing a profile measures request concurrency for that same actor/conversation; it does not test cross-user isolation. Each profile needs a restricted client credential, actor ID, and conversation ID. Profiles selected for Task SSE need distinct active `task_id` values; set `long_task: true` for exactly `-long-task-streams` of the first `-task-streams` expanded clients. The tool reports the configured long-task ratio and whether event streams remain open or finish early. Approval writes still require a different pending approval fixture for each decision.

Messages and approvals are disabled by default. To enable those writes, pass `-enable-writes` plus `-messages N`; each selected virtual user sends `-burst-size` consecutive messages (default 1), paced by the global `-write-rate`. Approval writes require a distinct pending approval fixture for each decision and `-approvals N -approval-decision approve|reject`. These writes can start real Tasks, incur model usage, and consume pending approvals. There are no automatic write retries.

The JSON report includes the task polling scope, sampled peak client goroutines/heap/system memory, and operation percentiles. Capture server CPU, memory, database, and model-provider use from the target environment's existing monitoring; this runner does not infer server capacity from client-side measurements. A mock-server pass is a protocol self-check only and does not count as production capacity evidence.
