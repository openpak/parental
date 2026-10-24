# Changelog — parental

`git log` stays the source of truth; this file is the readable summary.

## Unreleased (v0.1.0)

- First version: the console-neutral parental-controls core. Postgres with embedded
  migrations; supervision (owner and guardians, invite and accept), v1 restriction profiles,
  per-network overrides, device records with pairing codes and optimistic versions, usage
  ingestion with dedup and 30-day / 5000-event retention, an audit trail and the
  `parental_changed` event stream (poll and subscribe).
- gRPC `openpak.parental.v1.Parental` on 20171 with adapter keys bound to namespaces; HTTP on
  20170 behind the website's key and the user's account-core session (whoami).
- Follows the account core's event stream and purges deleted accounts.
- Device blobs are sealed at rest (AES-256-GCM, `PARENTAL_BLOB_KEY`, key id and nonce beside
  each ciphertext); blobs in clear are sealed at start. A start without the key is refused
  unless `PARENTAL_BLOB_INSECURE_PLAINTEXT=1` (development only).
