# Notes: parental

The console-neutral parental-controls core. Parental controls are **account-level data**
keyed by the account core's account id, the way the account core holds accounts, friends
and presence: who supervises whom, each supervised account's restriction profile, per-network
overrides, the devices under parental controls with their pairing records, the play-time
reports networks upload, and an audit trail of every change. Console protocols stay in the
adapters (`nx-pctl` for the Switch); nothing console-shaped is interpreted here.

Design, data model, event contract, per-network mapping and the nx-pctl cutover plan:
[docs/design.md](design.md).

## What exists, what does not

| Part | State |
| --- | --- |
| Supervision (owner + guardians, invite/accept), profiles, overrides, audit, events | built, integration tested (v0.1.0, unreleased) |
| Device records, pairing codes with TTL, usage ingestion, account-deletion purge | built, integration tested |
| nx-pctl adapter (`NX_PCTL_BACKEND=parental`) and importer | built and tested on nx-pctl's branch `parental-core-adapter`; **off by default, enabled nowhere** |
| Deriving a Switch console setting from a profile | not built (design section 8, decision 3) |
| Website / app screens on this API | not built |
| Wii U/3DS, Xbox, PS3 adapters | not built (TODO rows in the mapping table) |

## Rules

- A member accepts an owner invite; a guardian accepts its own invite. Pending grants nothing.
- Active supervisors edit the profile and overrides; the member reads its own profile.
  A member cannot leave an active supervision. The owner leaving ends it, restrictions included.
- A paired device's identity never changes; every device write names the version it read.
- Every change writes its audit row and its `parental_changed` event in its own transaction.
  The audit trail names fields, never values.
- Never logged: tokens, PINs, pairing codes, documents, usage payloads.
- Device blobs (they hold console PINs) are sealed at rest with AES-256-GCM under
  `PARENTAL_BLOB_KEY`, required at start; blobs still in clear are sealed at each start
  (docs/design.md section 7). `PARENTAL_BLOB_INSECURE_PLAINTEXT=1` is for development only.
- `account_deleted` on the account core's stream purges the account's parental data.
