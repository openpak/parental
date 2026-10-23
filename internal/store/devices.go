package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Device is one record under parental controls, as an adapter keeps it. The
// namespace always comes from the adapter's key.
type Device struct {
	ID             string
	Namespace      string
	Ref            string
	Owner          string
	Supervisors    []string // owner included
	Subject        string
	PairingCode    string
	PairingExpires int64 // unix; 0 = none
	PairedAt       int64 // unix; 0 = not paired
	State          []byte
	Version        uint64
	UpdatedAt      int64
}

const (
	// MaxStateBytes bounds the adapter blob.
	MaxStateBytes = 1 << 20
	// MaxDeviceSupervisors bounds a device's supervisor list.
	MaxDeviceSupervisors = 16
	// Usage retention: nx-pctl's own bounds (30 days, 5000 events a device).
	UsageDays   = 30
	UsageCap    = 5000
	MaxIngest   = 1000
	MaxUsageLen = 64 << 10
)

var refRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func (d *Device) validate() error {
	d.Owner = strings.ToLower(d.Owner)
	switch {
	case !refRe.MatchString(d.Ref):
		return invalid("device_ref must match [A-Za-z0-9._:-]{1,64}")
	case !ValidID(d.Owner):
		return invalid("owner_id must be an account id")
	case len(d.Subject) > 128:
		return invalid("subject is at most 128 bytes")
	case len(d.PairingCode) > 32:
		return invalid("pairing_code is at most 32 bytes")
	case len(d.State) > MaxStateBytes:
		return invalid("state_json is at most %d bytes", MaxStateBytes)
	}
	if len(d.State) == 0 {
		d.State = []byte("{}")
	}
	var m map[string]any
	if json.Unmarshal(d.State, &m) != nil || m == nil {
		return invalid("state_json must be a JSON object")
	}
	sup := []string{d.Owner}
	for _, a := range d.Supervisors {
		a = strings.ToLower(a)
		if !ValidID(a) {
			return invalid("supervisor_ids must be account ids")
		}
		if !slices.Contains(sup, a) {
			sup = append(sup, a)
		}
	}
	if len(sup) > MaxDeviceSupervisors {
		return invalid("at most %d supervisors per device", MaxDeviceSupervisors)
	}
	sort.Strings(sup[1:])
	d.Supervisors = sup
	return nil
}

const deviceCols = `id::text, namespace, device_ref, owner_id::text, COALESCE(subject,''), COALESCE(pairing_code,''),
	COALESCE(EXTRACT(EPOCH FROM pairing_expires_at)::bigint, 0), COALESCE(EXTRACT(EPOCH FROM paired_at)::bigint, 0),
	state, version, EXTRACT(EPOCH FROM updated_at)::bigint,
	ARRAY(SELECT account_id::text FROM device_supervisors s WHERE s.device_id = devices.id ORDER BY account_id <> devices.owner_id, account_id)`

func scanDevice(r pgx.Row) (Device, error) {
	var d Device
	err := r.Scan(&d.ID, &d.Namespace, &d.Ref, &d.Owner, &d.Subject, &d.PairingCode, &d.PairingExpires, &d.PairedAt,
		&d.State, &d.Version, &d.UpdatedAt, &d.Supervisors)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// changedKeys names the top-level keys of two JSON objects that differ.
func changedKeys(before, after []byte) []string {
	var a, b map[string]any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	out := []string{}
	for k, v := range b {
		if !reflect.DeepEqual(a[k], v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func unixOrNull(n int64) *time.Time {
	if n <= 0 {
		return nil
	}
	t := time.Unix(n, 0)
	return &t
}

// PutDevice creates (expected 0) or replaces a device record. A stale version,
// or 0 for a record that exists, is ErrConflict; a bound subject changing is
// ErrSubjectChange. The audit row names the blob keys that changed; a write that
// changed only "updatedAt" (a console's routine upload) writes no audit row
// and no event.
func (s *Store) PutDevice(ctx context.Context, actor string, d Device, expected uint64) (Device, error) {
	if err := d.validate(); err != nil {
		return Device{}, err
	}
	var out Device
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var id, subject string
		var version uint64
		var state []byte
		var before []string
		err := tx.QueryRow(ctx, `SELECT id::text, COALESCE(subject,''), version, state,
			ARRAY(SELECT account_id::text FROM device_supervisors s WHERE s.device_id = devices.id ORDER BY account_id)
			FROM devices WHERE namespace = $1 AND device_ref = $2 FOR UPDATE`, d.Namespace, d.Ref).
			Scan(&id, &subject, &version, &state, &before)
		created := errors.Is(err, pgx.ErrNoRows)
		switch {
		case created && expected != 0:
			return ErrConflict
		case created:
			err = tx.QueryRow(ctx, `INSERT INTO devices (namespace, device_ref, owner_id, subject, pairing_code,
				pairing_expires_at, paired_at, state) VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7, $8)
				RETURNING id::text`, d.Namespace, d.Ref, d.Owner, d.Subject, d.PairingCode,
				unixOrNull(d.PairingExpires), unixOrNull(d.PairedAt), d.State).Scan(&id)
			if uniqueViolation(err) {
				return ErrSubjectChange // another record of this namespace holds the subject
			}
			if err != nil {
				return err
			}
			state = []byte("{}")
		case err != nil:
			return err
		case version != expected:
			return ErrConflict
		case subject != "" && d.Subject != subject:
			return ErrSubjectChange
		default:
			_, err = tx.Exec(ctx, `UPDATE devices SET owner_id = $2, subject = NULLIF($3,''), pairing_code = NULLIF($4,''),
				pairing_expires_at = $5, paired_at = $6, state = $7, version = version + 1, updated_at = now()
				WHERE id = $1`, id, d.Owner, d.Subject, d.PairingCode, unixOrNull(d.PairingExpires), unixOrNull(d.PairedAt), d.State)
			if uniqueViolation(err) {
				return ErrSubjectChange
			}
			if err != nil {
				return err
			}
		}
		after := slices.Clone(d.Supervisors)
		sort.Strings(after)
		supChanged := !slices.Equal(before, after)
		if supChanged {
			if _, err := tx.Exec(ctx, `DELETE FROM device_supervisors WHERE device_id = $1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO device_supervisors (device_id, account_id) SELECT $1, unnest($2::uuid[])`,
				id, d.Supervisors); err != nil {
				return err
			}
		}
		keys := slices.DeleteFunc(changedKeys(state, d.State), func(k string) bool { return k == "updatedAt" })
		if created || supChanged || len(keys) > 0 || subject != d.Subject {
			action := "device.put"
			if created {
				action = "device.create"
			}
			if err := audit(ctx, tx, actor, action, "", id, d.Namespace, map[string]any{
				"fields": keys, "supervisors_changed": supChanged, "paired": d.Subject != "",
			}); err != nil {
				return err
			}
			if err := emit(ctx, tx, "device", d.Owner, d.Namespace, d.Ref, actor); err != nil {
				return err
			}
		}
		out, err = scanDevice(tx.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = $1`, id))
		return err
	})
	return out, err
}

// GetDevice is one record of the namespace.
func (s *Store) GetDevice(ctx context.Context, namespace, ref string) (Device, error) {
	return scanDevice(s.Pool.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE namespace = $1 AND device_ref = $2`, namespace, ref))
}

// FindBySubject is the record a paired device belongs to.
func (s *Store) FindBySubject(ctx context.Context, namespace, subject string) (Device, error) {
	if subject == "" {
		return Device{}, ErrNotFound
	}
	return scanDevice(s.Pool.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE namespace = $1 AND subject = $2`, namespace, subject))
}

// FindByPairingCode is the record whose code this is, while the code is live.
// Should two live records share a code, the newest wins.
func (s *Store) FindByPairingCode(ctx context.Context, namespace, code string) (Device, error) {
	if code == "" {
		return Device{}, ErrNotFound
	}
	return scanDevice(s.Pool.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices WHERE namespace = $1 AND pairing_code = $2
		AND pairing_expires_at > now() ORDER BY pairing_expires_at DESC LIMIT 1`, namespace, code))
}

// ListDevices pages the namespace's records by device_ref, 500 a page,
// optionally only those supervisor supervises.
func (s *Store) ListDevices(ctx context.Context, namespace, supervisor, after string) ([]Device, string, error) {
	const page = 500
	rows, err := s.Pool.Query(ctx, `SELECT `+deviceCols+` FROM devices WHERE namespace = $1 AND device_ref > $2
		AND ($3 = '' OR id IN (SELECT device_id FROM device_supervisors WHERE account_id::text = $3))
		ORDER BY device_ref LIMIT $4`, namespace, after, supervisor, page)
	if err != nil {
		return nil, "", err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) { return scanDevice(r) })
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == page {
		next = out[len(out)-1].Ref
	}
	return out, next, nil
}

// DeleteDevice removes a record with its supervisors and usage.
func (s *Store) DeleteDevice(ctx context.Context, actor, namespace, ref string, expected uint64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var id, owner string
		var version uint64
		err := tx.QueryRow(ctx, `SELECT id::text, owner_id::text, version FROM devices WHERE namespace = $1 AND device_ref = $2 FOR UPDATE`,
			namespace, ref).Scan(&id, &owner, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if version != expected {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `DELETE FROM devices WHERE id = $1`, id); err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, "device.delete", "", id, namespace, nil); err != nil {
			return err
		}
		return emit(ctx, tx, "device", owner, namespace, ref, actor)
	})
}

// DeviceSummary is a device as the parent view lists it: no blob, no PIN, no code.
type DeviceSummary struct {
	ID             string `json:"id"`
	Namespace      string `json:"namespace"`
	Ref            string `json:"device_ref"`
	Owner          bool   `json:"owner"`
	Paired         bool   `json:"paired"`
	PairingPending bool   `json:"pairing_pending"`
	PairedAt       int64  `json:"paired_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// SupervisedDevices is every device, any namespace, the account supervises.
func (s *Store) SupervisedDevices(ctx context.Context, account string) ([]DeviceSummary, error) {
	if !ValidID(account) {
		return []DeviceSummary{}, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT d.id::text, d.namespace, d.device_ref, d.owner_id = $1, d.subject IS NOT NULL,
		d.subject IS NULL AND d.pairing_expires_at > now(),
		COALESCE(EXTRACT(EPOCH FROM d.paired_at)::bigint, 0), EXTRACT(EPOCH FROM d.updated_at)::bigint
		FROM devices d JOIN device_supervisors s ON s.device_id = d.id
		WHERE s.account_id = $1 ORDER BY d.namespace, d.device_ref`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeviceSummary, error) {
		var v DeviceSummary
		err := r.Scan(&v.ID, &v.Namespace, &v.Ref, &v.Owner, &v.Paired, &v.PairingPending, &v.PairedAt, &v.UpdatedAt)
		return v, err
	})
}

// ---------- usage ----------

// UsageEvent is one report as a network sent it.
type UsageEvent struct {
	Payload    []byte          `json:"-"`
	Raw        json.RawMessage `json:"payload"`
	Kind       string          `json:"kind"`
	OccurredAt int64           `json:"occurred_at"`
	MemberID   string          `json:"member_id,omitempty"`
}

// Digest is what a report is deduplicated by.
func Digest(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return sum[:]
}

// IngestUsage stores the reports the device does not hold yet, prunes what is
// past retention, and returns how many were new and how many are held.
func (s *Store) IngestUsage(ctx context.Context, namespace, ref string, evs []UsageEvent) (int, int, error) {
	if len(evs) > MaxIngest {
		return 0, 0, invalid("at most %d events per call", MaxIngest)
	}
	for _, e := range evs {
		if len(e.Payload) > MaxUsageLen || !json.Valid(e.Payload) {
			return 0, 0, invalid("each payload must be JSON of at most %d bytes", MaxUsageLen)
		}
		if e.MemberID != "" && !ValidID(e.MemberID) {
			return 0, 0, invalid("member_id must be an account id")
		}
		if len(e.Kind) > 64 {
			return 0, 0, invalid("kind is at most 64 bytes")
		}
	}
	accepted, held := 0, 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `SELECT id::text FROM devices WHERE namespace = $1 AND device_ref = $2`, namespace, ref).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		for _, e := range evs {
			tag, err := tx.Exec(ctx, `INSERT INTO usage_events (device_id, member_id, occurred_at, kind, payload, digest)
				VALUES ($1, NULLIF($2,'')::uuid, $3, $4, $5::json, $6) ON CONFLICT (device_id, digest) DO NOTHING`,
				id, e.MemberID, unixOrNull(e.OccurredAt), e.Kind, string(e.Payload), Digest(e.Payload))
			if err != nil {
				return err
			}
			accepted += int(tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `DELETE FROM usage_events WHERE device_id = $1 AND occurred_at < now() - make_interval(days => $2)`,
			id, UsageDays); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM usage_events WHERE device_id = $1 AND id NOT IN
			(SELECT id FROM usage_events WHERE device_id = $1 ORDER BY id DESC LIMIT $2)`, id, UsageCap); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE device_id = $1`, id).Scan(&held)
	})
	return accepted, held, err
}

// ListUsage is a device's reports in ingestion order.
func (s *Store) ListUsage(ctx context.Context, namespace, ref string, since int64, limit int) ([]UsageEvent, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id::text FROM devices WHERE namespace = $1 AND device_ref = $2`, namespace, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.usage(ctx, id, since, limit)
}

// DeviceUsage is a device's reports for one of its supervisors (the parent
// view); anyone else gets ErrNotFound.
func (s *Store) DeviceUsage(ctx context.Context, account, deviceID string, since int64) ([]UsageEvent, error) {
	if !ValidID(account) || !ValidID(deviceID) {
		return nil, ErrNotFound
	}
	var ok bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM device_supervisors WHERE device_id = $1 AND account_id = $2)`,
		deviceID, account).Scan(&ok); err != nil || !ok {
		return nil, orNotFound(err)
	}
	return s.usage(ctx, deviceID, since, UsageCap)
}

func (s *Store) usage(ctx context.Context, deviceID string, since int64, limit int) ([]UsageEvent, error) {
	if limit <= 0 || limit > UsageCap {
		limit = UsageCap
	}
	rows, err := s.Pool.Query(ctx, `SELECT payload::text, kind, COALESCE(EXTRACT(EPOCH FROM occurred_at)::bigint, 0), COALESCE(member_id::text, '')
		FROM usage_events WHERE device_id = $1 AND ($2 = 0 OR occurred_at IS NULL OR occurred_at >= to_timestamp($2))
		ORDER BY id LIMIT $3`, deviceID, since, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageEvent, error) {
		var e UsageEvent
		var payload string
		err := r.Scan(&payload, &e.Kind, &e.OccurredAt, &e.MemberID)
		e.Payload = []byte(payload)
		e.Raw = e.Payload
		return e, err
	})
}
