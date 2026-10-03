# File and website preview deployment

The Web readers and file workspace share the authenticated HTTP file data plane.
Upgrade the Server and participating Nodes together: the Node advertises
`filePreviewV1`. Ordinary images/audio/video are streamed, including HTTP Range;
text loads 128 KiB at a time. Older Nodes retain their existing capability API
but advanced readers explicitly request an upgrade rather than aggregating binary
files through the JSON channel.

For static websites, configure each deployment separately:

```text
MIRA_NODE_PREVIEW_DOMAIN=preview.mira.example.test
*.preview.mira.example.test CNAME mira.example.test
```

The suffix must be dedicated to previews and differ from the console hostname.
Keep `MIRA_SECURE_COOKIES=true` on HTTPS deployments. The suffix is optional for
ordinary document/media/ZIP browsing. It is required for website previews.
Preview state is transient; restarting Server expires open sites.

Caddy needs the `dns.providers.alidns` module. A reviewable site block is:

```caddyfile
*.preview.mira.example.test {
    tls {
        dns alidns {
            access_key_id {env.ALIYUN_ACCESS_KEY_ID}
            access_key_secret {env.ALIYUN_ACCESS_KEY_SECRET}
        }
    }
    reverse_proxy 127.0.0.1:8787
}
```

Preserve the original Host in the proxy. Backend ingress stays loopback-only;
use the deployment's existing environment/credential file and service ownership.
The AliDNS credentials must remain available for DNS-01 renewal. One wildcard
certificate covers all single-label preview IDs; creating a site never calls DNS,
requests a site certificate or reloads Caddy. See the
[AliDNS module configuration](https://github.com/caddy-dns/alidns) and
[Caddy wildcard HTTPS documentation](https://caddyserver.com/docs/automatic-https#wildcard-certificates).
Do not use on-demand TLS for these preview IDs.

The current and beta deployment audit found two different preparation levels:
the Nix-managed ingress already has the AliDNS module and a private environment
file; beta's distribution Caddy needs that module and a DNS-01 credential source.
This repository intentionally records no production domains, addresses or
credentials. DNS write permission and wildcard issuance still require deployment
verification. Nix-owned configuration must be reviewed in its owning repository;
this change does not run `nixos-rebuild` or modify that repository.

Before enabling a deployment, validate Caddy's adapted config, DNS resolution of
a random preview label, the wildcard SAN, HTTPS/WSS relay and a real Node file
preview. Test two independent sites, root-relative assets/module/fetch, logout,
Node revocation and session expiry. TLS preparation and a Mira release are
separate deployment steps.

## Current boundaries

- Read-only static websites; no shell server, build tool or backend API starts.
- The HTML parent is the default root; directory actions can choose a wider
  explicit root and nested entry. Missing resources link to root/entry controls.
- Preview bootstrap grants last 60 seconds and are consumed once. Host-only
  HttpOnly cookies and the live administrator session authorize a 30-minute
  preview; owners can close/renew it before expiry. The scoped cookie may remain
  for 24 hours to support owner leases, but is never authorization beyond the
  Server session lifetime. Active streams recheck ownership every
  two seconds. Node disconnect/revocation closes data sockets immediately and invalidates the site control epoch.
- ZIP central-directory index budget: 32 MiB and 100,000 entries. This bounds
  index allocation; it is not an ordinary file-size ceiling. Invalid names and
  symlinks are omitted. Duplicate names require an explicit entry ordinal scoped to the archive version;
  site assets with duplicate names fail rather than silently choosing one.
- Compressed ZIP entries use a selected-entry cache for actual random reads.
  It is bounded to 1 GiB total and 64 entries, reuses completed entries, expires
  after five idle minutes, and cleans failed/canceled preparations and shutdown.
  Concurrent copies have one preparation owner. Stored entries read directly.
- Transport limits: 128 Server streams, 32 per Node, 64 KiB binary frames,
  20-second connect, two-minute preparation and 45-second data inactivity limits.
- Text/source and Markdown are incrementally loaded. Directory pages show 250
  rows with forward/first-page controls; expanding a folder loads its children.
  Source highlighting is deliberately lightweight; distant line references discard earlier scanned chunks. File content is current Node
  data, independent of historical typed image snapshots.
- Back/forward and reopening preserve reading position through a disposable
  cache of up to 12 documents with at most 2 MiB of text each, after rechecking
  file metadata. Larger documents remain readable in chunks; their complete
  loaded contents are not retained after closing the reader.
- Native decoding depends on the Web client. Android opens file windows in a
  separate WebView Activity with no `MiraAndroid` bridge; its original console
  remains warm. Device acceptance remains necessary before releasing an APK.
