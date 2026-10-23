# parental: design

Status: design for v0.1.0, written before the code (2026-10-23). Nothing here is deployed.

The console-neutral parental-controls core. It holds parental controls as **account-level
data** keyed by the account core's permanent account id, the way the account core holds
accounts, friends and presence: networks (Switch now; Wii U/3DS, Xbox, PS3 later) read what
applies to them and write back what their consoles report. Console protocols stay in the
adapters; nothing Nintendo-, Microsoft- or Sony-shaped is interpreted here.

## 1. Name, place, ports

| | |
| --- | --- |
| Repository | `github.com/openpak/parental` (private), checkout `platforms/core/infra/parental` |
| Module / image | `github.com/openpak/parental`, `ghcr.io/openpak/parental` |
| Language, storage | Go, Postgres (database `parental`), embedded migrations applied at start |
| Ports | block **20170**: 20170 HTTP, 20171 gRPC, 20172 metrics (reserved). 20160 is taken by `voice`, which is not yet in `docs/ports.md`; both rows still have to be added there |
| License | AGPL-3.0-only, like `account`, `chat` and `voice` |
| Federation | home only. A region node never holds parental data (`docs/federation.md`: nodes hold no accounts, no links) |

## 2. Data model

Every account id is the account core's UUID (`accounts.id`). Names, emails and birthdays are
never copied here; a surface that needs a name asks the account core.

### supervisions

Who supervises whom. One row per (member, supervisor).

| column | |
| --- | --- |
| `member_id` | the supervised account |
| `supervisor_id` | a parent or guardian account; never the member |
| `role` | `owner` (at most one per member: the account that set supervision up) or `guardian` (a further parent) |
| `state` | `pending` until the invited party accepts, then `active`. Only `active` rows grant anything |
| `invited_by`, `created_at`, `accepted_at` | |

Who accepts: for an `owner` row the **member** accepts (a parent cannot take over an account
by naming it); for a `guardian` row the **guardian** accepts. An account that is a member of
an active supervision cannot supervise anyone.

### profiles

One restriction profile per member: `member_id`, `doc` (JSON, schema below), `version`
(incremented on every write; optimistic concurrency), `updated_by`, `updated_at`.

Profile document, v1 (every key optional; absent means "not restricted"):

```json
{
  "rating_age": 12,
  "play_time": {"mon": 60, "tue": 60, "wed": 60, "thu": 60, "fri": 120, "sat": 180, "sun": 180},
  "bedtime": {"start": "21:00", "end": "07:00"},
  "titles": {
    "allow": [{"namespace": "switch", "title_id": "0100000000010000"}],
    "block": [{"namespace": "switch", "title_id": "01006a800016e000"}]
  },
  "communication": {
    "mode": "friends_only",
    "friend_requests": "blocked",
    "ugc_posting": false,
    "free_communication": "restricted",
    "free_communication_titles": [{"namespace": "switch", "title_id": "0100000000010000"}]
  },
  "store": {"purchases": "blocked", "spending_limit": {"amount_minor": 2000, "currency": "EUR", "period": "month"}}
}
```

Validation (refused with the failing path, nothing stored): unknown keys; `rating_age`
0..21; `play_time` keys `mon`..`sun`, minutes 0..1440; `bedtime` both times `HH:MM`; at
most 500 titles per list, `namespace` `[a-z0-9-]{1,32}`, `title_id` `[A-Za-z0-9._-]{1,64}`;
`communication.mode` `unrestricted|friends_only|none`; `friend_requests` `allowed|blocked`;
`free_communication` `allowed|restricted`; `store.purchases` `allowed|blocked`;
`spending_limit.amount_minor` 0..10^9, `currency` three upper-case letters, `period`
`day|week|month`.

### profile_overrides

Per-network overrides: (`member_id`, `namespace`) → `doc` (a JSON object, at most 64 KiB),
`version`, `updated_by`, `updated_at`. The core stores and versions it and never reads it;
the network named by `namespace` interprets it (for Switch: fields with no neutral meaning,
section 6). A network only ever reads the neutral profile plus its own override.

### devices, device_supervisors

A console (or other device) under parental controls, with its pairing record.

| column | |
| --- | --- |
| `id` | UUID, the core's own |
| `namespace` | the network (`switch`), from the adapter's key, never from the request |
| `device_ref` | the network's id for the record (Switch: the moon id, 16 hex digits); unique per namespace |
| `owner_id` | the managing supervisor account |
| `subject` | the device identity the console proves once paired (Switch: the dauth device id); unique per namespace; once set it can never change to another value |
| `pairing_code`, `pairing_expires_at` | the live pairing code and its deadline (Switch mints 6 digits, 15 min TTL). Lookup by code only matches while unexpired |
| `paired_at` | |
| `state` | adapter-owned JSON blob: the record as the network keeps it (Switch: nx-pctl's `PctlDevice`, without its events). Opaque here. It holds the console PIN (section 7) |
| `version` | optimistic concurrency: every write names the version it read |

`device_supervisors` (`device_id`, `account_id`) lists every supervisor of a device (owner
plus further parents), as the adapter reports them. That is what lets the parent view and the
account-deletion purge find a device without reading the blob.

### usage_events

Play-time and state reports ingested from networks, one row per event: `device_id`,
`member_id` (when the network knows which account played; null for Switch, whose events
name console users, not accounts), `occurred_at` (null when the event carried no time),
`kind`, `payload` (the event as the network sent it, kept byte for byte), `digest`
(SHA-256 of the payload; unique per device, so a retried upload is not counted twice).
Retention: 30 days and at most 5000 events per device (nx-pctl's own bounds), pruned on
ingest.

### audit_log

Every change, written in the same transaction as the change: `at`, `actor` (an account
UUID, `network:<namespace>` for an adapter, or `system`), `action`, `member_id`,
`device_id`, `namespace`, `detail` (JSON: which fields changed, versions; **never** a PIN,
a pairing code, a profile document or a name). Readable by the member's active supervisors.

### events, event_counter, core_cursor

The parental event stream (section 4) and the cursor of the account-core stream it consumes.

## 3. API

### gRPC `openpak.parental.v1.Parental` (port 20171, network adapters)

Every call presents an adapter key in `x-internal-key`. `PARENTAL_ADAPTER_KEYS` binds each
key to one namespace (`switch=…,wiiu=…`), as `voice` does; the namespace comes from the
key, so an adapter can only read or write its own network's devices.

| RPC | |
| --- | --- |
| `GetSupervision(account_id)` | active supervisions where the account is member or supervisor |
| `GetProfile(member_id)` | the profile (or none), the caller namespace's override, and whether the member is supervised |
| `PutDevice(device, expected_version)` | create (`expected_version` 0) or replace a device record and its supervisors. `ABORTED` on a version mismatch or when the record exists and 0 was given; `FAILED_PRECONDITION` when `subject` would change |
| `GetDevice(device_ref)` | |
| `FindDevice(subject \| pairing_code)` | the paired device, or the record whose code is still live |
| `ListDevices(supervisor_id?, page_token)` | the namespace's records, 500 a page |
| `DeleteDevice(device_ref, expected_version)` | removes the device, its supervisors and its usage |
| `IngestUsage(device_ref, events[])` | stores new events (dedup by digest), prunes, returns how many were new and how many are held |
| `ListUsage(device_ref, since_unix, limit)` | oldest first, ingestion order |
| `PollEvents(since_version)`, `SubscribeEvents(since_version)` | the parental stream |

### HTTP (port 20170, website and app)

`GET /healthz` is open. Every other route needs **both** `X-Internal-Key:
$PARENTAL_INTERNAL_KEY` (the service is internal; the website proxies it) **and**
`Authorization: Bearer <account-core session>`. The session is resolved by the account core's
own `GET /v1/whoami`; anything but `status: active` is 401. The caller is always that
account; no route takes "who am I" from the body.

| route | who may |
| --- | --- |
| `GET /v1/me/supervision` | anyone: their own rows, both directions, pending included |
| `POST /v1/me/members` `{"member_id"}` | invites the member with the caller as `owner` (pending). The member must exist and be active in the account core |
| `POST /v1/me/members/{member}/guardians` `{"account_id"}` | the member's active owner; invites a guardian (pending) |
| `POST /v1/me/supervisions/{member}/{supervisor}/accept` | the invited party (section 2) |
| `DELETE /v1/me/supervisions/{member}/{supervisor}` | the active owner (removing itself ends the whole supervision of that member); a guardian removing itself; the invited party declining a pending row. A member cannot remove an active supervision |
| `GET /v1/me/members/{member}/profile` | the member's active supervisors, and the member (read only) |
| `PUT /v1/me/members/{member}/profile` `{"profile", "version"?}` | active supervisors; `version` given and stale is 409 |
| `PUT`/`DELETE /v1/me/members/{member}/overrides/{namespace}` `{"doc"}` | active supervisors |
| `GET /v1/me/members/{member}/audit?limit=` | active supervisors |
| `GET /v1/me/devices` | the devices the caller supervises (summary, no blob, no PIN, no code) |
| `GET /v1/me/devices/{id}/usage?days=` | supervisors of that device |

A member or device the caller may not see is a 404, never a 403, so ids cannot be probed.

## 4. Event contract

Durable, versioned, at-least-once, deduplicated by `event_id`, the same shape and the same
counter-row ordering as the account core's `invalidation_events` (version order equals commit
order). One type:

| field | |
| --- | --- |
| `type` | `parental_changed` |
| `scope` | `supervision`, `profile`, `override`, `device` |
| `account_id` | who it concerns: the member (supervision, profile, override) or the device owner (device). A supervision change is emitted once for the member and once for the supervisor |
| `namespace` | set for `override` and `device` |
| `subject_id` | the other party (supervision), or the `device_ref` (device) |
| `origin` | who wrote it (`network:switch`, or the acting account id), so an adapter can skip its own writes |

Events say *what* changed, never the data; receivers re-read through the API. Kept 7 days
(`PARENTAL_EVENT_RETENTION`); a consumer offline longer re-reads everything it holds.

The events are **not** placed on the account core's stream: its `EmitEvent` admits a closed
set of types and `parental_changed` is not in it (section 8, decision 1).

## 5. Account-core integration

- **Who is calling (HTTP):** `GET {PARENTAL_CORE_HTTP_URL}/v1/whoami` with the caller's bearer.
  Existing API, no change.
- **Does this account exist (invites):** `Identity.GetAccount` over gRPC with
  `PARENTAL_CORE_INTERNAL_KEY`. Existing API.
- **Lifecycle:** `Events.SubscribeAccountEvents` from a cursor kept in `core_cursor`. On
  `account_deleted` the account's parental data is purged in one transaction: its
  supervisions (either side), profile, overrides, the devices it owns (with their usage),
  its `device_supervisors` rows, and the audit rows about it as a member or by it as actor.
  `account_banned`/`account_unbanned` change nothing: a banned parent's child stays
  supervised, a banned child stays restricted. Existing API.
- **Links:** not needed yet. A network maps its own subject to an account through the account
  core's `Links` itself, as it already does.

No change to the account core is required for v0.1.0.

## 6. Per-network mapping

The Switch console setting is **per console**, the neutral profile is **per member**. In v0.1.0
the Switch setting document stays where nx-pctl keeps it (the device blob), unchanged, and is
not derived from a profile (section 8, decision 3). The table is the mapping a derivation
would use.

| neutral field | Switch (pctl moon setting, nx-pctl) | Wii U / 3DS | Xbox / 360 | PS3 |
| --- | --- | --- | --- | --- |
| `rating_age` | `functionalRestrictionLevel: CUSTOM`, `customSettings.ageRestriction {restriction: RESTRICTED, ratingAge}`; `ratingOrganization` from the console's region (override) | TODO | TODO | TODO |
| `play_time.<day>` | `playTimerRegulations.timerMode: EACH_DAY_OF_THE_WEEK`, `eachDayOfTheWeekRegulations.<day>.timeToPlayInOneDay {enabled, limitTime}`. **The console caps `limitTime` at 360 minutes**; a larger neutral limit has no Switch form | TODO | TODO | TODO |
| `bedtime` | `eachDayOfTheWeekRegulations.<day>.bedtime {enabled, startingTime, endingTime}`, `bedtimeStartingTime` | TODO | TODO | TODO |
| `titles.allow` | `whitelistedApplications.<title id>.safeLaunch: ALLOW` | TODO | TODO | TODO |
| `titles.block` | no pctl field (the console restricts by rating only) | TODO | TODO | TODO |
| `communication.ugc_posting` | `customSettings.snsPostRestriction` | TODO | TODO | TODO |
| `communication.free_communication(_titles)` | `customSettings.ugcRestriction {restriction, applications.<title id>.restriction}` | TODO | TODO | TODO |
| `communication.mode`, `friend_requests` | no pctl field; would be enforced by the Switch friends adapter (nx-baas), not by the console | TODO | TODO | TODO |
| `store.*` | no pctl field; would be enforced by nx-shop | TODO | TODO | TODO |
| Switch-only, override `switch` | `playTimerRegulations.restrictionMode` (ALARM / FORCED_TERMINATION), `customSettings.vrRestriction`, `ratingOrganization` | | | |
| device-only, never in a profile | the console PIN (`unlockCode`), players, owners, notification token: the device blob | | | |

The shapes are nx-pctl's `pctlSettingSchema` (the console's parse table, docs study §5b).
This service never builds a Switch document; the console-facing wire contract is
`docs/switch/parental-controls-firmware-2026-09-26.md` and stays nx-pctl's.

## 7. Privacy rules

Parental data is about minors.

- No names, emails or birthdays are stored; ids only.
- Never logged at any level: tokens, PINs, pairing codes, profile or override documents,
  usage payloads. Logged at info level: counts, versions, namespaces, short device refs;
  never an account id beside a name.
- The device blob carries the console PIN in clear, as nx-pctl's file does today (the parent
  surface shows it). Database access is therefore the same trust level as nx-pctl's data
  volume (section 8, decision 5).
- Tests and fixtures use made-up UUIDs and made-up names only.

## 8. Decisions the owner must make before a production cutover

1. **Account-core stream.** Keep `parental_changed` on this service's own stream (as built),
   or add it to the account core's closed `KnownTypes` + constraint (an additive migration)
   so adapters hear it on the stream they already follow.
2. **Consent and release.** Members accept an owner invite and cannot leave an active
   supervision. Should an adult (by birthday in the account core) be able to leave, and
   should an account under 13 skip acceptance?
3. **Per console vs per child (Switch).** Keep editing the console document per device (as
   today), or derive it from one member's profile (which member, when a console has several
   users?). Also: what a neutral play-time limit over 360 minutes becomes on a Switch.
4. **Website cutover.** When the parent screens start using this service for supervision and
   profiles, and whether nx-pctl's `/internal/pctl` stays the Switch device surface.
5. **PIN at rest.** Accept the PIN in clear in the blob (status quo), or encrypt the blob
   with a service key.
6. **Ports.** Register 20170 (and voice's 20160) in `docs/ports.md`.

## 9. nx-pctl cutover plan

nx-pctl keeps answering `/moon/v1` exactly as now; only where its records live changes.

1. **v0.1.0 (this change).** `parental` exists; nx-pctl gains `NX_PCTL_BACKEND=parental`
   (default empty: the file store, behaviour unchanged) and a one-shot importer. Nothing is
   enabled anywhere.
2. **Rehearsal.** Run `parental` against a scratch database, import a *copy* of production's
   `pctl.json` with `nx-pctl -import-parental <copy>`, start an nx-pctl against it with the
   flag on, and compare `/internal/pctl/devices` and the served setting ETags with the file
   build's. Expected: identical.
3. **Cutover.** Stop nx-pctl, run the importer against the live file, start nx-pctl with the
   flag on. The parental core becomes the source of truth for Switch device records;
   nx-pctl keeps an in-memory copy (one instance only, as today) and still writes
   `pctl.json` after every change as a mirror. At its first start with the flag it keeps a
   copy `pctl.json.before-parental`, and it refuses to start when the core holds no Switch
   records but the file does (the importer has not run).
4. **Rollback.** Unset the flag and restart: the mirror is the current state. Records changed
   in the core by anything other than nx-pctl (an account deletion) are re-read at their
   next write by version conflict; while the flag is off they are not.
5. **Later.** nx-pctl subscribes to `SubscribeEvents` to reload a device another writer
   changed, and the profile → Switch derivation (decision 3), once the website edits through
   this service.

How the flag-on adapter writes: after every change it compares each record with what it last
wrote (blob digest) and sends `PutDevice` with the version it holds, then `IngestUsage` with
the events the core has not seen; a record that left the map is `DeleteDevice`d. A version
conflict (someone else changed or removed it) re-reads that record from the core. A core that
does not answer leaves the change in memory and in the mirror, and the next change retries it.
