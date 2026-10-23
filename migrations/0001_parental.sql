-- The parental-controls core (docs/design.md section 2). Every account id is the
-- account core's UUID; no names, emails or birthdays live here.

CREATE TABLE supervisions (
    member_id     UUID NOT NULL,
    supervisor_id UUID NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('owner', 'guardian')),
    state         TEXT NOT NULL CHECK (state IN ('pending', 'active')),
    invited_by    UUID NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    accepted_at   TIMESTAMPTZ,
    PRIMARY KEY (member_id, supervisor_id),
    CHECK (member_id <> supervisor_id)
);
-- At most one owner per member, pending or active.
CREATE UNIQUE INDEX supervisions_one_owner ON supervisions (member_id) WHERE role = 'owner';
CREATE INDEX supervisions_by_supervisor ON supervisions (supervisor_id);

CREATE TABLE profiles (
    member_id  UUID PRIMARY KEY,
    doc        JSONB NOT NULL,
    version    BIGINT NOT NULL DEFAULT 1,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE profile_overrides (
    member_id  UUID NOT NULL,
    namespace  TEXT NOT NULL,
    doc        JSONB NOT NULL CHECK (jsonb_typeof(doc) = 'object'),
    version    BIGINT NOT NULL DEFAULT 1,
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (member_id, namespace)
);

CREATE TABLE devices (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace          TEXT NOT NULL,
    device_ref         TEXT NOT NULL,
    owner_id           UUID NOT NULL,
    subject            TEXT,
    pairing_code       TEXT,
    pairing_expires_at TIMESTAMPTZ,
    paired_at          TIMESTAMPTZ,
    state              JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(state) = 'object'),
    version            BIGINT NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, device_ref)
);
CREATE UNIQUE INDEX devices_subject ON devices (namespace, subject) WHERE subject IS NOT NULL;
CREATE INDEX devices_pairing ON devices (namespace, pairing_code) WHERE pairing_code IS NOT NULL;
CREATE INDEX devices_owner ON devices (owner_id);

CREATE TABLE device_supervisors (
    device_id  UUID NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    PRIMARY KEY (device_id, account_id)
);
CREATE INDEX device_supervisors_by_account ON device_supervisors (account_id);

-- payload is json, not jsonb: it is kept byte for byte, so its digest and the
-- bytes an adapter reads back are the ones it sent.
CREATE TABLE usage_events (
    id          BIGSERIAL PRIMARY KEY,
    device_id   UUID NOT NULL REFERENCES devices (id) ON DELETE CASCADE,
    member_id   UUID,
    occurred_at TIMESTAMPTZ,
    kind        TEXT NOT NULL DEFAULT '',
    payload     JSON NOT NULL,
    digest      BYTEA NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (device_id, digest)
);
CREATE INDEX usage_events_member ON usage_events (member_id, occurred_at) WHERE member_id IS NOT NULL;

-- No foreign keys: the trail outlives the device it names.
CREATE TABLE audit_log (
    id        BIGSERIAL PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor     TEXT NOT NULL,
    action    TEXT NOT NULL,
    member_id UUID,
    device_id UUID,
    namespace TEXT,
    detail    JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_log_member ON audit_log (member_id, id) WHERE member_id IS NOT NULL;

-- The parental stream: the account core's counter-row pattern, so version
-- order equals commit order and a poller never skips an event.
CREATE TABLE event_counter (
    id INT PRIMARY KEY CHECK (id = 1),
    n  BIGINT NOT NULL
);
INSERT INTO event_counter (id, n) VALUES (1, 0);

CREATE TABLE events (
    version    BIGINT PRIMARY KEY,
    event_id   UUID NOT NULL DEFAULT gen_random_uuid(),
    type       TEXT NOT NULL CHECK (type IN ('parental_changed')),
    scope      TEXT NOT NULL CHECK (scope IN ('supervision', 'profile', 'override', 'device')),
    account_id UUID NOT NULL,
    namespace  TEXT,
    subject_id TEXT,
    origin     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_created ON events (created_at);

-- Where this service is in the account core's event stream.
CREATE TABLE core_cursor (
    id            INT PRIMARY KEY CHECK (id = 1),
    since_version BIGINT NOT NULL
);
INSERT INTO core_cursor (id, since_version) VALUES (1, 0);
