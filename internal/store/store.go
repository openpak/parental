// Package store is the parental core's Postgres state and its rules: who may
// change what is checked inside the same transaction that changes it, and every
// change writes its audit row and its parental_changed event in that
// transaction too, so nothing is announced or recorded that did not commit.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openpak/parental/internal/blobseal"
	"github.com/openpak/parental/migrations"
)

var (
	ErrNotFound = errors.New("parental: not found")
	// ErrConflict is a stale version, or a row that already exists.
	ErrConflict = errors.New("parental: conflict")
	// ErrSubjectChange is a bound device being rebound to another identity.
	ErrSubjectChange = errors.New("parental: a paired device cannot change its identity")
)

// InvalidError is a request the rules refuse; its text is safe to return.
type InvalidError struct{ Msg string }

func (e InvalidError) Error() string { return e.Msg }

func invalid(format string, a ...any) error { return InvalidError{Msg: fmt.Sprintf(format, a...)} }

type Store struct {
	Pool *pgxpool.Pool
	hub  *hub
	// blob seals the device blobs at rest (blobseal); nil keeps them in clear
	// (PARENTAL_BLOB_INSECURE_PLAINTEXT=1, development and tests only).
	blob *blobseal.Key
}

// Open connects, applies the embedded migrations and, with a key, seals the device blobs
// still in clear (sealDevices). A nil key keeps blobs in clear.
func Open(ctx context.Context, url string, key *blobseal.Key) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{Pool: pool, hub: newHub(), blob: key}
	n, err := s.sealDevices(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if n > 0 {
		log.Printf("store: sealed %d device blob(s) kept in clear under key %s", n, key.ID)
	}
	return s, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies embedded SQL migrations in filename order, once each (the
// account core's scheme).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("store: migration table: %w", err)
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("store: apply %s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, name)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidID is what an account id from the account core looks like.
func ValidID(s string) bool { return uuidRe.MatchString(s) }

// tx runs fn in a transaction and wakes the event subscribers after it commits.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if err := pgx.BeginFunc(ctx, s.Pool, fn); err != nil {
		return err
	}
	s.hub.wake()
	return nil
}

// audit records one change. detail must never carry a PIN, a pairing code, a
// document or a name: field names and versions only.
func audit(ctx context.Context, tx pgx.Tx, actor, action, member, device, namespace string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor, action, member_id, device_id, namespace, detail)
		VALUES ($1, $2, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid, NULLIF($5,''), $6)`,
		actor, action, member, device, namespace, b)
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// AuditEntry is one row of the trail as a supervisor reads it.
type AuditEntry struct {
	At        time.Time      `json:"at"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	DeviceID  string         `json:"device_id,omitempty"`
	Namespace string         `json:"namespace,omitempty"`
	Detail    map[string]any `json:"detail"`
}

// Audit is the trail about a member, newest first, for one of its active
// supervisors. Anyone else gets ErrNotFound.
func (s *Store) Audit(ctx context.Context, actor, member string, limit int) ([]AuditEntry, error) {
	if ok, err := s.isActiveSupervisor(ctx, s.Pool, member, actor); err != nil || !ok {
		return nil, orNotFound(err)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT at, actor, action, COALESCE(device_id::text,''), COALESCE(namespace,''), detail
		FROM audit_log WHERE member_id = $1 ORDER BY id DESC LIMIT $2`, member, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		err := r.Scan(&e.At, &e.Actor, &e.Action, &e.DeviceID, &e.Namespace, &e.Detail)
		return e, err
	})
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return ErrNotFound
}

// ---------- the parental stream ----------

// Event is one parental_changed.
type Event struct {
	Version   uint64
	EventID   string
	Type      string
	Scope     string
	AccountID string
	Namespace string
	SubjectID string
	Origin    string
	CreatedAt int64
}

// emit records one event inside the caller's transaction. The counter row lock
// is held until commit, so version order is commit order.
func emit(ctx context.Context, tx pgx.Tx, scope, account, namespace, subject, origin string) error {
	_, err := tx.Exec(ctx, `WITH v AS (UPDATE event_counter SET n = n + 1 WHERE id = 1 RETURNING n)
		INSERT INTO events (version, type, scope, account_id, namespace, subject_id, origin)
		SELECT v.n, 'parental_changed', $1, $2, NULLIF($3,''), NULLIF($4,''), $5 FROM v`,
		scope, account, namespace, subject, origin)
	if err != nil {
		return fmt.Errorf("store: event: %w", err)
	}
	return nil
}

// Poll returns events after since, oldest first, capped at 1000.
func (s *Store) Poll(ctx context.Context, since uint64, limit int) ([]Event, uint64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `SELECT version, event_id::text, type, scope, account_id::text, COALESCE(namespace,''),
		COALESCE(subject_id,''), origin, EXTRACT(EPOCH FROM created_at)::bigint
		FROM events WHERE version > $1 ORDER BY version LIMIT $2`, since, limit)
	if err != nil {
		return nil, since, err
	}
	evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.Version, &e.EventID, &e.Type, &e.Scope, &e.AccountID, &e.Namespace, &e.SubjectID, &e.Origin, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, since, err
	}
	max := since
	if len(evs) > 0 {
		max = evs[len(evs)-1].Version
	}
	return evs, max, nil
}

// Subscribe streams every event after since, then stays live until ctx ends.
// At-least-once: correctness rests on the re-poll, the wake only shortens it.
func (s *Store) Subscribe(ctx context.Context, since uint64, send func(Event) error) error {
	for {
		for {
			evs, max, err := s.Poll(ctx, since, 500)
			if err != nil {
				return err
			}
			for _, e := range evs {
				if err := send(e); err != nil {
					return err
				}
			}
			since = max
			if len(evs) < 500 {
				break
			}
		}
		wake := s.hub.wait()
		tick := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			tick.Stop()
			return ctx.Err()
		case <-wake:
			tick.Stop()
		case <-tick.C:
		}
	}
}

// PruneEvents drops events older than keep.
func (s *Store) PruneEvents(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM events WHERE created_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int64(keep.Seconds())))
	return tag.RowsAffected(), err
}

type hub struct {
	mu sync.Mutex
	ch chan struct{}
}

func newHub() *hub { return &hub{ch: make(chan struct{})} }

func (h *hub) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	close(h.ch)
	h.ch = make(chan struct{})
}

func (h *hub) wait() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ch
}

// ---------- the account core's stream ----------

// CoreCursor is where this service is in the account core's event stream.
func (s *Store) CoreCursor(ctx context.Context) (uint64, error) {
	var v uint64
	err := s.Pool.QueryRow(ctx, `SELECT since_version FROM core_cursor WHERE id = 1`).Scan(&v)
	return v, err
}

func (s *Store) SetCoreCursor(ctx context.Context, v uint64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE core_cursor SET since_version = $1 WHERE id = 1 AND since_version < $1`, v)
	return err
}

// PurgeAccount removes a deleted account's parental data in one transaction
// (docs/design.md section 5): its supervisions on either side, its profile and
// overrides, the devices it owns with their usage, its device-supervisor rows,
// and the audit rows about it or by it. Each remaining counterpart of a removed
// supervision hears a supervision event.
func (s *Store) PurgeAccount(ctx context.Context, account string) error {
	if !ValidID(account) {
		return nil
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		// The account owning a member's supervision is the owner leaving: the whole
		// supervision ends, guardians and restrictions with it (as in Remove).
		rows, err := tx.Query(ctx, `WITH owned AS (SELECT member_id FROM supervisions WHERE supervisor_id = $1 AND role = 'owner'),
			p AS (DELETE FROM profiles WHERE member_id IN (SELECT member_id FROM owned)),
			o AS (DELETE FROM profile_overrides WHERE member_id IN (SELECT member_id FROM owned))
			DELETE FROM supervisions WHERE member_id IN (SELECT member_id FROM owned) AND supervisor_id <> $1
			RETURNING supervisor_id::text`, account)
		if err != nil {
			return err
		}
		guardians, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `DELETE FROM supervisions WHERE member_id = $1 OR supervisor_id = $1
			RETURNING CASE WHEN member_id = $1 THEN supervisor_id ELSE member_id END::text`, account)
		if err != nil {
			return err
		}
		others, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		others = append(others, guardians...)
		for _, q := range []string{
			`DELETE FROM profiles WHERE member_id = $1`,
			`DELETE FROM profile_overrides WHERE member_id = $1`,
			`DELETE FROM devices WHERE owner_id = $1`,
			`DELETE FROM device_supervisors WHERE account_id = $1`,
			`DELETE FROM audit_log WHERE member_id = $1 OR actor = $1::text`,
		} {
			if _, err := tx.Exec(ctx, q, account); err != nil {
				return err
			}
		}
		for _, o := range others {
			if err := emit(ctx, tx, "supervision", o, "", "", "system"); err != nil {
				return err
			}
		}
		return audit(ctx, tx, "system", "account.purged", "", "", "", map[string]any{"supervisions": len(others)})
	})
}
