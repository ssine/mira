# Mira Server endpoint discovery v1

A configured Mira Server URL is the durable identity anchor for one Node credential. A deployment
may advertise alternate ingress URLs without rebinding that identity by serving this unauthenticated
document at `<configured-server-url>/.well-known/mira`:

```json
{
  "version": 1,
  "endpoints": [
    { "url": "https://direct.mira.example:24443", "priority": 10, "scope": "direct" },
    { "url": "https://mira.example", "priority": 20, "scope": "global" }
  ]
}
```

Lower numeric priorities are preferred. `scope` is descriptive in v1 and does not bypass probing.
The document is optional: a missing, invalid, oversized, redirected or unreachable response leaves
the client on its configured URL. Clients accept at most 16 endpoints and 64 KiB of JSON.

Before sending a credential to an alternate endpoint, the client performs an unauthenticated,
non-redirecting `GET <endpoint>/healthz`. The endpoint is eligible only when it returns HTTP 200 and
a bounded JSON body containing `{"status":"ok"}`. Discovery and probes each have a three-second
deadline. A configured HTTPS URL may advertise only HTTPS alternatives, and endpoint URLs may not
contain user information, a query, a fragment or control characters.

The Node worker keeps using its selected healthy endpoint for enrollment, registration, heartbeat,
the reverse capability channel and target-side SSH transport. A transport failure, HTTP 502/503/504,
or reverse-channel failure advances to the next advertised endpoint; retrying Node control requests
is safe because those operations are idempotent. The configured URL is always appended as a fallback
even if the document omits it.

Selection and failure state are process-local derived state. The Node configuration and protected
identity file retain the configured URL, so changes to the discovery document take effect after a
worker restart without credential rotation or an identity migration. Older clients ignore the
well-known document and continue using the configured URL.
