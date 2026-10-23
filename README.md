# OpenPak parental

The console-neutral parental-controls core. Parental controls are **account-level data**
keyed by the account core's account id, the way the account core holds accounts, friends
and presence: who supervises whom, each supervised account's restriction profile, per-network
overrides, the devices under parental controls with their pairing records, the play-time
reports networks upload, and an audit trail of every change. Console protocols stay in the
adapters (`nx-pctl` for the Switch); nothing console-shaped is interpreted here.

Design, data model, event contract, per-network mapping and the nx-pctl cutover plan:
[docs/design.md](docs/design.md).

## What exists, what does not

| Part | State |
| --- | --- |
| Supervision (owner + guardians, invite/accept), profiles, overrides, audit, events | built, integration tested (v0.1.0, unreleased) |
| Device records, pairing codes with TTL, usage ingestion, account-deletion purge | built, integration tested |
| nx-pctl adapter (`NX_PCTL_BACKEND=parental`) and importer | built and tested on nx-pctl's branch `parental-core-adapter`; **off by default, enabled nowhere** |
| Deriving a Switch console setting from a profile | not built (design section 8, decision 3) |
| Website / app screens on this API | not built |
| Wii U/3DS, Xbox, PS3 adapters | not built (TODO rows in the mapping table) |

## gRPC (`openpak.parental.v1.Parental`, port 20171)

For network adapters. Every call presents an adapter key in `x-internal-key`; the key names
the namespace (`PARENTAL_ADAPTER_KEYS`, `switch=…`), and every device call is scoped to it.
`GetSupervision`, `GetProfile`, `PutDevice`, `GetDevice`, `FindDevice`, `ListDevices`,
`DeleteDevice`, `IngestUsage`, `ListUsage`, `PollEvents`, `SubscribeEvents`. The comments in
`proto/openpak/parental/v1/parental.proto` are the contract. Regenerate the stubs with

    protoc -I proto --go_out=. --go_opt=module=github.com/openpak/parental \
      --go-grpc_out=. --go-grpc_opt=module=github.com/openpak/parental proto/openpak/parental/v1/parental.proto

## HTTP (port 20170)

For the website and app. Every route but `GET /healthz` needs the website's
`X-Internal-Key` **and** the user's account-core session as `Authorization: Bearer`, resolved
by the core's `GET /v1/whoami` (only `active` accounts). Routes are in design section 3; an id
the caller may not see is 404.

## Rules

- A member accepts an owner invite; a guardian accepts its own invite. Pending grants nothing.
- Active supervisors edit the profile and overrides; the member reads its own profile.
  A member cannot leave an active supervision. The owner leaving ends it, restrictions included.
- A paired device's identity never changes; every device write names the version it read.
- Every change writes its audit row and its `parental_changed` event in its own transaction.
  The audit trail names fields, never values.
- Never logged: tokens, PINs, pairing codes, documents, usage payloads.
- `account_deleted` on the account core's stream purges the account's parental data.

## Run

```sh
cp example.env .env   # fill in; secrets as *_FILE
go run ./cmd/parental
```

Needs Postgres and the account core (gRPC and HTTP). Ports: 20170 HTTP, 20171 gRPC,
20172 reserved (block 20170; not yet in the port plan (https://docs.openpak.org/contributors/ports/)).

## Tests

```sh
podman run -d --name openpak-parental-test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=parental_test \
  -p 127.0.0.1:54339:5432 docker.io/library/postgres:16-alpine
PARENTAL_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:54339/parental_test?sslmode=disable' \
  GOWORK=off go test -race -count=1 ./...
```

The integration tests drop and recreate the `public` schema of that database and run against
a stand-in account core (whoami over HTTP, Identity and Events over bufconn). Without the
variable they skip; the profile validator and key parser tests always run.

## Repository

Module `github.com/openpak/parental`, image `ghcr.io/openpak/parental` (static, scratch,
uid 10001). Release by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`; tags and
manual dispatch only). `internal/accountpb` is a copy of the account core's `account` and
`events` contracts, the way `chat` and `voice` carry them; refresh by re-copying.

## License

AGPL-3.0-only.
