# Claude Code protocol probe

This optional experiment uses the official SDK and its bundled Claude Code
subprocess with a **local simulated model API**. No Anthropic key, subscription
login, real model request, Mira identity file or production service is needed.
All working files live in a disposable OS temporary directory. It is separate
from Mira's Go runtime and release packaging.

```sh
npm ci --prefix experiments/claude-code
MIRA_CLAUDE_TEST_DATABASE_URL=postgresql://mira:claude-test@127.0.0.1:55433/mira \
  npm test --prefix experiments/claude-code
```

Use your own disposable PostgreSQL on loopback. For example:

```sh
docker run --rm --name mira-claude-probe-db \
  -e POSTGRES_USER=mira -e POSTGRES_PASSWORD=claude-test -e POSTGRES_DB=mira \
  -p 127.0.0.1:55433:5432 postgres:17-alpine
```

Wait for the database to be ready and run the tests from another terminal. Stop
that test container when finished. The tests create and drop uniquely named
`claude_probe_*` schemas, never Mira production tables. When the database variable
is absent, the MCP test runs and the two PostgreSQL/SDK tests are explicitly
skipped. A complete run reports **3 passed, 0 skipped**.

The tests cover:

- official MCP client interoperability, complete schemas, image blocks and no
  retries after failed tool requests;
- raw transcript preservation (including escaped NUL), UUID deduplication and
  subagent key isolation;
- real Claude subprocess streaming/tool dispatch, mirror writes, session resume
  after deleting local history, and observable mirror failure without aborting
  model execution.

Set `MIRA_CLAUDE_PROBE_DEBUG=1` to include the temporary SDK debug log on failure.
Logs contain only this probe's synthetic conversation. The probe always supplies
isolated configuration and synthetic authentication to the loopback model API.

`mira-tools.mjs` is the prototype runtime adapter. `postgres-store.mjs` is a
disposable test backend with a subset of optional SessionStore operations. A
production Claude Node must call a Mira storage API with its Node credential,
not connect to PostgreSQL directly. Neither this adapter nor this probe registers
Claude as a managed Mira runtime or enables it in the Web UI.

See [integration findings and next steps](../../docs/claude-code-integration.md).
