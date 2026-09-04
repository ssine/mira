export async function loginAdmin(
  serverUrl,
  username = "admin",
  password = process.env.MIRA_TEST_ADMIN_PASSWORD ?? "mira-local-admin-password",
) {
  const response = await fetch(`${serverUrl}/v1/admin/login`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ username, password }),
  });
  const body = await response.json();
  if (!response.ok) throw new Error(`administrator login failed: ${response.status} ${JSON.stringify(body)}`);
  const cookie = response.headers.get("set-cookie")?.split(";", 1)[0];
  if (!cookie || !body.csrfToken) throw new Error("administrator login returned an incomplete session");
  return { cookie, csrfToken: body.csrfToken };
}

export async function adminRequest(serverUrl, session, pathname, options = {}) {
  const response = await fetch(`${serverUrl}${pathname}`, {
    ...options,
    headers: {
      cookie: session.cookie,
      "x-mira-csrf": session.csrfToken,
      "content-type": "application/json",
      ...(options.headers ?? {}),
    },
  });
  const body = await response.json();
  if (!response.ok) throw new Error(`${pathname}: ${response.status} ${JSON.stringify(body)}`);
  return body;
}

export async function approvePendingNode(
  serverUrl,
  session,
  nodeKey,
  timeoutMs = 30_000,
) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const enrollments = await adminRequest(
      serverUrl,
      session,
      "/v1/admin/enrollments?status=pending",
    );
    const pending = enrollments.data.find((item) => item.nodeKey === nodeKey);
    if (pending) {
      return adminRequest(
        serverUrl,
        session,
        `/v1/admin/enrollments/${pending.enrollmentId}/approve`,
        { method: "POST", body: "{}" },
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for enrollment request from ${nodeKey}`);
}

export function appServerWebSocket(serverUrl, nodeToken, nodeId, storeId = null) {
  const query = storeId ? `?storeId=${encodeURIComponent(storeId)}` : "";
  return new WebSocket(
    `${serverUrl.replace(/^http/, "ws")}/v1/nodes/${nodeId}/app-server${query}`,
    ["mira-client-v1", `auth.${Buffer.from(nodeToken).toString("base64url")}`],
  );
}
