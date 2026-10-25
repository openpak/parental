# Parental Controls Server

OpenPak's Parental Controls Server.

| | |
| --- | --- |
| Network | Console-neutral; Switch adapter `nx-pctl` |
| Role | Parental-controls core: supervision, restriction profiles, device records, play-time usage, audit |
| Depends on | [`account`](https://github.com/openpak/account), PostgreSQL |
| Image | `ghcr.io/openpak/parental` |
| Licence | AGPL-3.0-only |

## What it serves

| Endpoint | What |
| --- | --- |
| gRPC, port 20171 | Network adapters: supervision, profiles, devices, usage and events |
| HTTP, port 20170 | Website and app: parental-control routes for a signed-in user; `GET /healthz` is open |

## Run

```sh
cp example.env .env   # fill in; secrets as *_FILE
go run ./cmd/parental
```

## Docs

- [Protocol](docs/protocol.md)
- [Evidence](docs/evidence.md)
- [Deploy](docs/deploy.md)
- [Notes](docs/notes.md)
- [Design](docs/design.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE).
