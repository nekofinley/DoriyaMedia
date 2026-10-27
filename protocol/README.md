# Native Client Protocol Contract

This directory is the source of truth for DoriyaMedia Android and Windows client contracts.

## Contract Status

- S1 publishes `GET /api/v1/meta` and the shared stable error envelope.
- S2 publishes and implements the OAuth-like device authorization, PKCE S256, rotating token, refresh-family revocation, CSRF/same-origin approval protection, email-verification gate, and device-management contract.
- S3 publishes and implements the playback ticket, server-proxy, server-HLS, and independent subtitle-authorization contracts.
- The S3 baseline transports are `direct-http`, `server-proxy`, and `server-hls`.
- S5 adds `direct-dash` as a default-disabled, capability-gated additive transport under contract version `1`. It is issued only when the server flag, device policy, multipart source eligibility, and `client.supports_dash_merge=true` all agree.
- The S5 server contract and default-off implementation are landed, but real Bilibili dual-stream playback, client-side merge, and the Windows/Android device/network matrices are not yet verified; S5 acceptance remains open.
- `local-relay` is not implemented and remains outside this contract revision; `local_relay_enabled` is discovery-only.
- S3.5 and S5 keep contract version `1` and are additive. OpenAPI document revision `1.5.0` adds the direct-dash schemas and fixture and records the optional `supports_dash_merge` client capability; clients that omit it remain compatible and cannot receive direct-dash.
- Server-HLS does not expose a query-driven master playlist in this revision. It publishes a WebVTT rendition URL under the same `ms_` path, and every rendition request reauthorizes both that media session and an independent `ss_` subtitle session.
- Existing `/api` routes and Web UI behavior are not part of this contract.

## Compatibility Matrix

The values below are the contract fixture baseline. A server reports its actual minimum versions through `/api/v1/meta`.

| Contract version | Windows minimum | Android minimum | Status |
| --- | --- | --- | --- |
| 1 | 0.1.0 | 0.1.0 | S1/S2 implemented; S3 playback and S3.5 subtitle authorization implemented; S5 direct-dash server contract implemented default-off, with client/device acceptance still open |

## Files

- `openapi.yaml`: OpenAPI 3.1 description of S1, S2, the S3 playback endpoints, the S3.5 subtitle chain, and the additive S5 direct-dash schemas.
- `schemas/meta.schema.json`: Exact `/api/v1/meta` response body.
- `schemas/error.schema.json`: Stable error envelope and closed error-code enum.
- `schemas/device-*.schema.json`: Device authorization requests, responses, states, and metadata.
- `schemas/auth-*.schema.json`: Token-pair and authenticated-user response shapes.
- `schemas/playback-ticket.schema.json`: Closed safe GET projection; it cannot carry a transport description.
- `schemas/playback-ticket-issuance.schema.json`: One-time create/refresh response; only `consuming` can carry a transport.
- `schemas/playback-ticket-create-request.schema.json`: Closed ticket-create request.
- `schemas/playback-client-capabilities.schema.json`: Client capability declaration.
- `schemas/playback-ticket-state.schema.json`: Closed eight-state ticket state machine.
- `schemas/playback-transport-kind.schema.json`: Closed v1 transport set, including the optional `direct-dash` kind.
- `schemas/transport*.schema.json`: The four v1 transport strategies.
- `schemas/direct-dash-stream.schema.json`: Closed per-track stream for the S5 direct video/audio pair.
- `schemas/media-session.schema.json`: Media-session expiry metadata; no separate capability field.
- `schemas/subtitle.schema.json`: Stable subtitle description, optional controlled WebVTT URL, and PGS-without-URL rule.
- `schemas/stream-headers.schema.json`: Complete outbound direct-http and direct-dash header whitelist.
- `fixtures/*.json`: Golden examples for server, Kotlin, and Rust tests.

## Meta Contract

The `/api/v1/meta` response has exactly four top-level fields:

- `contract_version`: integer `1`.
- `server_version`: non-empty string; `dev` in the golden fixture.
- `minimum_client_versions`: required `windows` and `android` strings.
- `features`: all required boolean flags, with no additional flags.

Feature flags are all `false` in the fixture until a deployment explicitly enables them:

- `client_v1_enabled`
- `device_auth_enabled`
- `playback_ticket_enabled`
- `direct_http_enabled`
- `server_proxy_v1_enabled`
- `server_hls_v1_enabled`
- `direct_dash_enabled`
- `local_relay_enabled`
- `native_player_enabled`
- `libmpv_fallback_enabled`

`direct_dash_enabled` gates the S5 direct-dash contract and defaults to false. The client must still request it with `supports_dash_merge=true`; the flag alone does not make direct-dash eligible. `local_relay_enabled` remains a discovery flag for future work and does not enable a path or transport in this contract.

## Device Auth Contract

### Endpoints

| Method | Path | Authentication | Result |
| --- | --- | --- | --- |
| `POST` | `/api/v1/auth/device/authorizations` | public | `201` authorization plus one-time `device_code` |
| `GET` | `/api/v1/auth/device/authorizations/{id}` | public | public `state` and `expires_at` only |
| `GET` | `/api/v1/auth/device/authorizations/lookup?user_code=...` | Web Cookie or Web Bearer session | safe approval-page metadata |
| `POST` | `/api/v1/auth/device/authorizations/{id}/decision` | Web Cookie or Web Bearer session | `approve` becomes `authorized`; `deny` becomes `denied` |
| `POST` | `/api/v1/auth/device/token` | public | `200` token pair or `202` pending |
| `POST` | `/api/v1/auth/refresh` | public; refresh token in JSON body | rotated token pair |
| `POST` | `/api/v1/auth/logout` | device Bearer token | revoke the current token family |
| `GET` | `/api/v1/devices` | device Bearer token | safe device list |
| `DELETE` | `/api/v1/devices/{id}` | device Bearer token | revoke one device |

Servers must reserve the literal `lookup` route before the `{id}` route.

### State Machine

Authorization states are `pending | authorized | denied | consumed | expired`.

- `authorized` is exchangeable exactly once.
- `denied`, `consumed`, and `expired` are terminal.
- While pending, token polling returns HTTP `202`, not an error.

### Browser Approval Security

- Lookup requires a current Web session and returns `Cache-Control: no-store`.
- While `state=pending`, lookup returns a one-time `csrf_token`; every successful pending lookup rotates it and invalidates the previous token. Non-pending lookup responses omit `csrf_token`.
- The CSRF token expires after five minutes, no later than the authorization itself. It is a credential and must never be logged or persisted by clients beyond the approval page flow.
- Decision requires `Origin` plus `X-CSRF-Token`. `Origin` must exactly match the request's effective scheme and Host, with no userinfo, path, query, or fragment.
- Decision may omit `Sec-Fetch-Site`; if supplied, its only accepted value is `same-origin`.
- A missing, malformed, incorrect, expired, or lookup-rotated CSRF token returns HTTP `403` with `forbidden` and `details.action=reload_authorization`. The page must lookup again before retrying.
- A repeated decision after the authorization was successfully decided returns HTTP `409` with `authorization_consumed`. This is distinct from a stale CSRF token and requires starting a new authorization when the user needs another approval.

### Tokens And Isolation

- Access tokens live for 900 seconds.
- Refresh tokens use a sliding 30-day (`2592000` second) window and rotate on every successful refresh.
- A refresh-token family has an absolute 180-day (`15552000` second) lifetime from initial issuance. Each rotation refreshes the sliding deadline only up to that absolute deadline.
- `refresh_expires_in` is therefore variable: integer seconds in the inclusive range `0..2592000`, rather than a fixed constant.
- The server stores only hashes of device codes and access/refresh tokens.
- Reusing an old refresh token returns `refresh_reuse_detected` and revokes the token family.
- When email verification is required by configuration and an account has a non-empty, unverified email address:
  - Lookup and decision invalidate the live Web session and return `401 unauthenticated`.
  - Device-code exchange returns `403 forbidden` without creating a device or token.
  - Device Bearer operations return `403 forbidden`.
  - Refresh returns `401 refresh_token_invalid` and revokes the family.
- An empty email address is exempt from the email-verification gate. This behavior preserves legacy accounts.
- The gate requires no new stable error code: `unauthenticated`, `forbidden`, and `refresh_token_invalid` already cover the real handler responses.
- Native device Bearer tokens and Web sessions are separate credential domains.
- Lookup and decision accept only the Web session alternatives.
- Logout, device management, and playback ticket control accept only device Bearer authentication.
- The server-proxy media endpoint accepts neither bearer nor cookie credentials; its path capability is the only credential.

## Playback Tickets

### Endpoints

| Method | Path | Authentication | Result |
| --- | --- | --- | --- |
| `POST` | `/api/v1/playback/tickets` | device Bearer | `201` created/issuance or `202` probing |
| `GET` | `/api/v1/playback/tickets/{id}` | device Bearer | safe status only |
| `POST` | `/api/v1/playback/tickets/{id}/refresh` | device Bearer | one-time next transport or terminal state |
| `GET` | `/api/v1/playback/media-sessions/{id}/stream` | `ms_` path capability | full or single-range server-proxy bytes |
| `HEAD` | `/api/v1/playback/media-sessions/{id}/stream` | `ms_` path capability | headers only |
| `GET` | `/api/v1/playback/media-sessions/{id}/index.m3u8` | `ms_` path capability | server-HLS media playlist |
| `GET` | `/api/v1/playback/media-sessions/{id}/{segment}` | `ms_` path capability | one server-HLS MPEG-TS segment |
| `GET`/`HEAD` | `/api/v1/playback/subtitle-sessions/{id}/subtitle.vtt` | `ss_` path capability | authorized WebVTT bytes or headers only |
| `GET`/`HEAD` | `/api/v1/playback/media-sessions/{id}/subtitle-sessions/{subtitle}` | matching `ms_` and `ss_` path capabilities | server-HLS WebVTT rendition |

### Create Request

The create request is closed and contains only:

- `file_id`: positive integer.
- `device_id`: consistency check; the access token remains the identity source.
- `client`: platform, version, engine, containers, codecs, subtitle codecs, optional `supports_dash_merge`, Range support, and safe-header support.
- `preferred_transports`: a unique subset/order of up to four transport kinds from `direct-http`, `server-proxy`, `server-hls`, and `direct-dash`.

The client cannot submit a source URL, source headers, mode, burn flag, force switch, media-session capability, or access/refresh token. Preferences only reorder transports that remain eligible; server authorization, device policy, and feature flags decide eligibility. Omitting `supports_dash_merge` is equivalent to false.

### State Machine

The ticket states are exactly:

`probing | ready | consuming | unsupported | failed | expired | revoked | exhausted`

- `probing`: ticket exists; no media or transport description is available.
- `ready`: server eligibility is known; no transport secret has been issued.
- `consuming`: a one-time transport description was released by create or refresh.
- `unsupported`: no transport in the current v1 contract is compatible.
- `failed`: probing or source preparation failed.
- `expired`: the ticket's absolute five-minute lifetime ended.
- `revoked`: user, device, policy, or emergency-stop state invalidated the ticket.
- `exhausted`: every eligible transport has been claimed or attempted.

### One-Time Issuance

- Create and successful refresh use `playback-ticket-issuance.schema.json`.
- Create may return `201` with terminal `unsupported` or `failed`; those are
  issuance states, not refresh outcomes. A terminal refresh uses the stable
  error envelope instead.
- Only `state=consuming` permits a `transport` property.
- A direct-http description contains a source URL only in this one-time response.
- A direct URL or direct-dash stream cannot be revoked after delivery. `source_expires_at` is reserved for a known source expiry, but the current server leaves it absent; clients must not depend on it.
- `GET` uses `playback-ticket.schema.json`, which has no `transport`, `streams`, `url`, `headers`, `media_session`, `subtitles`, capability, or `source_expires_at` property.
- `GET` never probes, claims, rebuilds, or reissues a description. It returns
  state, media summary, and `remaining_transports` even while the ticket is
  `probing` or `consuming`.
- Refresh first reads the ticket and applies a pure local state gate before any
  file, Driver, Engine, or source authorization. Only `ready` and `consuming`
  may claim the next transport.
- Refresh never probes the source and never reconstructs an already issued
  transport. It uses the media summary and candidate order frozen at creation,
  while rechecking the current transport feature flags.
- A probing ticket returns `409 invalid_request` with
  `details.action=status`. The client must poll `GET` and try refresh only after
  the state becomes `ready`; refresh never completes probing.
- Refresh of `unsupported`, `failed`, `expired`, or `exhausted` returns
  `410 ticket_expired` with `details.action=request_new_ticket`.
  Refresh of `revoked` returns `403 forbidden` with
  `details.action=reauthorize`.
- Every successful refresh releases exactly one transport description. The
  candidate that produces it is consumed exactly once. A candidate is
  attempted at most once; if minting that candidate fails, the same refresh
  may advance through stored fallbacks before returning the single successful
  description.
- Concurrent refresh attempts must use a server-side conditional claim. Exactly
  one may obtain a transport description; clients must serialize refresh
  requests (single-flight). A losing concurrent claim returns
  `409 invalid_request` with `details.action=retry`.
- The server does not persist plaintext source transports, direct URLs, media
  capabilities, or issued descriptions. A restarted or later request cannot
  restore a description that was already released.
- `remaining_transports` never repeats the transport just issued.
- A consuming transport may include `subtitles`. Each entry has a stable `sub_<local-id>` id, language, label, protocol format (`vtt`, `srt`, `ass`, or `pgs`), optional controlled URL, default flag, and forced flag.
- Text tracks that can be delivered safely are converted to WebVTT and receive an independent `ss_` URL. PGS is descriptive only in this contract revision, has no URL, and is omitted from the server-HLS subtitle list because this revision has no burn-in path.

### Transports

| Kind | Required shape |
| --- | --- |
| `direct-http` | One HTTPS `muxed` stream, allowlisted headers only |
| `server-proxy` | Media-session expiries plus one server-hosted `/stream` URL |
| `server-hls` | Media-session expiries plus one server-hosted `/index.m3u8` URL |
| `direct-dash` | Exactly one HTTPS `video/mp4` stream and one HTTPS `audio/mp4` stream, each with its own allowlisted headers; no media session |

Server media sessions use a 15-minute idle lifetime and a 6-hour absolute cap. Ticket expiry and media-session expiry are independent.

The direct-dash MVP sets `range=false` on both streams because source Range support is not yet verified. Both URLs must be HTTPS without userinfo, and either stream failing URL, MIME, role, or header validation makes the entire candidate unavailable. The ticket can fall back to server-HLS only when `server_hls_v1_enabled=true`; otherwise it continues to the next eligible transport or `unsupported`. The client must merge the two streams locally; direct-dash never creates a media session. Client-side merge and real dual-stream playback are not yet verified, so this rule is not evidence that S5 acceptance is closed.

### Media Capability

- The high-entropy `ms_` media capability appears only in the media-session URL path.
- It is never accepted from a query parameter, cookie, Authorization header, or separate JSON field.
- `media-session.schema.json` therefore exposes only expiry metadata.
- Access logs, metrics, traces, and crash diagnostics must redact the `{id}` path segment before recording a media-session request.
- The OpenAPI operation declares this requirement through `x-media-session-capability` with `log_redaction: true`.

### Subtitle Authorization Chain

- Probe completion snapshots only subtitle ids belonging to the ticket file into the immutable ticket grant. The snapshot is deduplicated, sorted, bounded to 32 ids, and accepts no source path, URL, title, or header.
- Each deliverable text track receives its own `playback_subtitle_sessions` row. The database stores only the SHA-256 hash of the high-entropy `ss_` capability and binds it to ticket, user, device, file, purpose, transport, subtitle id, and `start_seconds`.
- Subtitle sessions use a 15-minute idle lifetime and a 6-hour absolute cap with version/CAS updates. The timeline origin is fixed in the session; clients cannot change it with a query.
- Capability routes accept only `GET` and `HEAD`. Any query string, including a bare trailing `?`, is rejected before authorization or source access.
- Authorization rechecks the live user status, email-verification gate, device revocation, and ticket `state=ready|consuming`/non-revoked state. It also requires the ticket's current `selected_transport` to match the subtitle session's bound transport. Ticket revocation, a transport fallback or other selected-transport change, device revocation, all-device revocation, feature shutdown, and user deletion fail closed.
- `ss_` appears only in the path and is redacted by log middleware. Source paths, source URLs, source headers, ffmpeg output, and backend errors never appear in responses or logs.
- For server-HLS, the WebVTT rendition URL requires both the issuing `ms_` session and its matching `ss_` session. Both must agree on ticket, user, device, and file; either session expiring or being revoked fails closed.

## Header Whitelist

Client-facing direct-http and direct-dash stream headers are limited to:

- `Referer`
- `Origin`
- `User-Agent`
- `Accept`
- `Accept-Language`

The schema rejects all other names. `Authorization`, `Cookie`, proxy credentials, access tokens, refresh tokens, and source-specific custom headers are never delivered in `stream.headers`. `Origin` must be an origin only, without a path, query, or fragment.

## Errors

All errors use:

```json
{
  "error": {
    "code": "ticket_expired",
    "message": "Display text",
    "details": {}
  }
}
```

Clients branch only on `error.code`. S3 requires these stable codes (`ticket_expired` was already reserved by S1):

- `transport_disabled`
- `ticket_not_found`
- `ticket_expired`
- `media_session_not_found`
- `media_session_expired`

The S1 and S2 codes remain present. `authorization_pending` remains intentionally absent because pending uses HTTP `202`.

## Media Summary

`media.audio` may be an empty array so video-only media can be represented. A non-empty entry declares codec and channel count; language is optional.

## Fixture Safety

- All fixture hosts use reserved `.example` names or loopback.
- Fixtures contain no real source URL, Cookie, Authorization value, Basic credential, token, IP address, or media-session capability; synthetic `ms_` placeholders appear only in media-session URL paths.
- `device-auth-lookup.json` carries a clearly synthetic pending CSRF placeholder, while `error-csrf-invalid.json` represents every `forbidden/reload_authorization` CSRF failure.
- `error-email-verification-required.json` demonstrates the existing `forbidden` response for device-code exchange or device Bearer access; refresh reuses the existing `refresh_token_invalid` fixture.
- Example ticket IDs and media-session path values are obvious placeholders.
- The safe GET status fixture contains no transport or URL shape.
- Direct-http, server-proxy, server-hls, and direct-dash transports each have a `consuming` issuance fixture. The direct-dash fixture proves the static additive contract only; it is not evidence of real dual-stream playback, client-side merge, or S5 acceptance.
- `protocol/fixtures/media-samples.json` is owned by S0 and explicitly skipped by the checker.

## Validation

Run from the repository root:

```powershell
.\scripts\check-protocol.ps1
git diff --check
```

The checker validates required schema/fixture files, the complete endpoint set, JSON Schema draft 2020-12 fixtures, `$ref` targets, S2 CSRF conditions and headers, token-lifetime bounds, email-gate mappings, S3/S5 state/error/transport sets, direct-dash video/audio pair constraints, safe GET projection, forbidden create-request fields, empty-audio support, media path capability, and GET/HEAD media response shape.
