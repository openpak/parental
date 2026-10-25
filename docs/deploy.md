# Deploy: parental

## Run

```sh
cp example.env .env   # fill in; secrets as *_FILE
go run ./cmd/parental
```

Needs Postgres and the account core (gRPC and HTTP). Ports: 20170 HTTP, 20171 gRPC,
20172 reserved (block 20170; not yet in the port plan (https://docs.openpak.org/contributors/ports/)).

## Build

Regenerate the stubs with

    protoc -I proto --go_out=. --go_opt=module=github.com/openpak/parental \
      --go-grpc_out=. --go-grpc_opt=module=github.com/openpak/parental proto/openpak/parental/v1/parental.proto

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
