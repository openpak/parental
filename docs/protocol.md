# Protocol: parental

## gRPC (`openpak.parental.v1.Parental`, port 20171)

For network adapters. Every call presents an adapter key in `x-internal-key`; the key names
the namespace (`PARENTAL_ADAPTER_KEYS`, `switch=…`), and every device call is scoped to it.
`GetSupervision`, `GetProfile`, `PutDevice`, `GetDevice`, `FindDevice`, `ListDevices`,
`DeleteDevice`, `IngestUsage`, `ListUsage`, `PollEvents`, `SubscribeEvents`. The comments in
`proto/openpak/parental/v1/parental.proto` are the contract.

## HTTP (port 20170)

For the website and app. Every route but `GET /healthz` needs the website's
`X-Internal-Key` **and** the user's account-core session as `Authorization: Bearer`, resolved
by the core's `GET /v1/whoami` (only `active` accounts). Routes are in design section 3; an id
the caller may not see is 404.
