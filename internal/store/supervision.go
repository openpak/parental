package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Supervision is one row: who supervises whom, in which role and state.
type Supervision struct {
	MemberID     string     `json:"member_id"`
	SupervisorID string     `json:"supervisor_id"`
	Role         string     `json:"role"`
	State        string     `json:"state"`
	InvitedBy    string     `json:"invited_by"`
	CreatedAt    time.Time  `json:"created_at"`
	AcceptedAt   *time.Time `json:"accepted_at,omitempty"`
}

// MaxSupervisors bounds the supervisors of one member, owner included.
const MaxSupervisors = 8

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) isActiveSupervisor(ctx context.Context, q querier, member, actor string) (bool, error) {
	if !ValidID(member) || !ValidID(actor) {
		return false, nil
	}
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM supervisions
		WHERE member_id = $1 AND supervisor_id = $2 AND state = 'active')`, member, actor).Scan(&ok)
	return ok, err
}

// CanView is the member itself or one of its active supervisors.
func (s *Store) CanView(ctx context.Context, actor, member string) (bool, error) {
	if actor == member && ValidID(actor) {
		return true, nil
	}
	return s.isActiveSupervisor(ctx, s.Pool, member, actor)
}

// isSupervised: the account is the member of an active supervision.
func isSupervised(ctx context.Context, q querier, account string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM supervisions WHERE member_id = $1 AND state = 'active')`, account).Scan(&ok)
	return ok, err
}

func isSupervising(ctx context.Context, q querier, account string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM supervisions WHERE supervisor_id = $1 AND state = 'active')`, account).Scan(&ok)
	return ok, err
}

func uniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// supervisionChanged is the audit row and the two events every supervision
// change writes: one for the member, one for the supervisor.
func supervisionChanged(ctx context.Context, tx pgx.Tx, actor, action, member, supervisor, role string) error {
	if err := audit(ctx, tx, actor, action, member, "", "", map[string]any{"supervisor": supervisor, "role": role}); err != nil {
		return err
	}
	if err := emit(ctx, tx, "supervision", member, "", supervisor, actor); err != nil {
		return err
	}
	return emit(ctx, tx, "supervision", supervisor, "", member, actor)
}

// Invite makes actor the owner of member's supervision: pending until the
// member accepts it, or active at once when linked (an under-13 member needs no
// acceptance, design section 8). The caller checked with the account core that
// member exists and, for linked, that member is under 13.
func (s *Store) Invite(ctx context.Context, actor, member string, linked bool) error {
	if !ValidID(actor) || !ValidID(member) {
		return invalid("member_id must be an account id")
	}
	if actor == member {
		return invalid("an account cannot supervise itself")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if sup, err := isSupervised(ctx, tx, actor); err != nil || sup {
			return errOr(err, invalid("a supervised account cannot supervise"))
		}
		state, accepted := "pending", "NULL"
		if linked {
			if busy, err := isSupervising(ctx, tx, member); err != nil || busy {
				return errOr(err, invalid("an account that supervises others cannot be supervised"))
			}
			state, accepted = "active", "now()"
		}
		_, err := tx.Exec(ctx, `INSERT INTO supervisions (member_id, supervisor_id, role, state, invited_by, accepted_at)
			VALUES ($1, $2, 'owner', $3, $2, `+accepted+`)`, member, actor, state)
		if uniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		action := "supervision.invite"
		if linked {
			action = "supervision.link"
		}
		return supervisionChanged(ctx, tx, actor, action, member, actor, "owner")
	})
}

func errOr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// AddGuardian lets member's active owner invite a further supervisor.
func (s *Store) AddGuardian(ctx context.Context, actor, member, guardian string) error {
	if !ValidID(guardian) {
		return invalid("account_id must be an account id")
	}
	if guardian == member {
		return invalid("an account cannot supervise itself")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `SELECT role FROM supervisions WHERE member_id = $1 AND supervisor_id = $2 AND state = 'active'`,
			member, actor).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != "owner") {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if sup, err := isSupervised(ctx, tx, guardian); err != nil || sup {
			return errOr(err, invalid("a supervised account cannot supervise"))
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM supervisions WHERE member_id = $1`, member).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSupervisors {
			return invalid("at most %d supervisors per account", MaxSupervisors)
		}
		_, err = tx.Exec(ctx, `INSERT INTO supervisions (member_id, supervisor_id, role, state, invited_by)
			VALUES ($1, $2, 'guardian', 'pending', $3)`, member, guardian, actor)
		if uniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		return supervisionChanged(ctx, tx, actor, "supervision.invite", member, guardian, "guardian")
	})
}

// Accept turns a pending row active. Only the invited party may: the member for
// an owner row, the guardian for a guardian row.
func (s *Store) Accept(ctx context.Context, actor, member, supervisor string) error {
	if !ValidID(member) || !ValidID(supervisor) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var role, state string
		err := tx.QueryRow(ctx, `SELECT role, state FROM supervisions WHERE member_id = $1 AND supervisor_id = $2 FOR UPDATE`,
			member, supervisor).Scan(&role, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		invitee := member
		if role == "guardian" {
			invitee = supervisor
		}
		if actor != invitee || state != "pending" {
			return ErrNotFound
		}
		if role == "owner" {
			if busy, err := isSupervising(ctx, tx, member); err != nil || busy {
				return errOr(err, invalid("an account that supervises others cannot be supervised"))
			}
		} else if sup, err := isSupervised(ctx, tx, supervisor); err != nil || sup {
			return errOr(err, invalid("a supervised account cannot supervise"))
		}
		if _, err := tx.Exec(ctx, `UPDATE supervisions SET state = 'active', accepted_at = now()
			WHERE member_id = $1 AND supervisor_id = $2`, member, supervisor); err != nil {
			return err
		}
		return supervisionChanged(ctx, tx, actor, "supervision.accept", member, supervisor, role)
	})
}

// Remove ends one supervision row, under the rules of docs/design.md section 3:
// the active owner removes anyone (itself: the whole supervision, profile and
// overrides with it); a guardian or a pending owner withdraws itself; the
// member declines a pending row. A member cannot remove an active supervision.
func (s *Store) Remove(ctx context.Context, actor, member, supervisor string) error {
	if !ValidID(member) || !ValidID(supervisor) || !ValidID(actor) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var role, state string
		err := tx.QueryRow(ctx, `SELECT role, state FROM supervisions WHERE member_id = $1 AND supervisor_id = $2 FOR UPDATE`,
			member, supervisor).Scan(&role, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var actorIsOwner bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM supervisions WHERE member_id = $1 AND supervisor_id = $2
			AND role = 'owner' AND state = 'active')`, member, actor).Scan(&actorIsOwner); err != nil {
			return err
		}
		switch {
		case actorIsOwner && supervisor == actor:
			// The owner leaving ends the supervision: nobody is left who may
			// manage it, so the restrictions go with it.
			rows, err := tx.Query(ctx, `DELETE FROM supervisions WHERE member_id = $1 RETURNING supervisor_id::text`, member)
			if err != nil {
				return err
			}
			gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM profiles WHERE member_id = $1`, member); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM profile_overrides WHERE member_id = $1`, member); err != nil {
				return err
			}
			if err := audit(ctx, tx, actor, "supervision.end", member, "", "", map[string]any{"supervisors": len(gone)}); err != nil {
				return err
			}
			if err := emit(ctx, tx, "supervision", member, "", actor, actor); err != nil {
				return err
			}
			for _, g := range gone {
				if err := emit(ctx, tx, "supervision", g, "", member, actor); err != nil {
					return err
				}
			}
			return nil
		case actorIsOwner,
			actor == supervisor && (role == "guardian" || state == "pending"),
			actor == member && state == "pending":
			if _, err := tx.Exec(ctx, `DELETE FROM supervisions WHERE member_id = $1 AND supervisor_id = $2`, member, supervisor); err != nil {
				return err
			}
			return supervisionChanged(ctx, tx, actor, "supervision.remove", member, supervisor, role)
		}
		return ErrNotFound
	})
}

// Mine is every row the account is on, either side, pending included.
func (s *Store) Mine(ctx context.Context, account string) ([]Supervision, error) {
	return s.supervisions(ctx, `(member_id = $1 OR supervisor_id = $1)`, account)
}

// Active is the active rows where the account is member or supervisor.
func (s *Store) Active(ctx context.Context, account string) ([]Supervision, error) {
	return s.supervisions(ctx, `(member_id = $1 OR supervisor_id = $1) AND state = 'active'`, account)
}

func (s *Store) supervisions(ctx context.Context, where, account string) ([]Supervision, error) {
	if !ValidID(account) {
		return []Supervision{}, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT member_id::text, supervisor_id::text, role, state, invited_by::text, created_at, accepted_at
		FROM supervisions WHERE `+where+` ORDER BY created_at, member_id, supervisor_id`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Supervision, error) {
		var v Supervision
		err := r.Scan(&v.MemberID, &v.SupervisorID, &v.Role, &v.State, &v.InvitedBy, &v.CreatedAt, &v.AcceptedAt)
		return v, err
	})
}

// ---------- profiles and overrides ----------

// Doc is a stored document and its version (0: none was ever set).
type Doc struct {
	JSON      []byte
	Version   uint64
	UpdatedAt time.Time
}

// Profile is the member's profile; JSON is "{}" when none was set.
func (s *Store) Profile(ctx context.Context, member string) (Doc, error) {
	return s.doc(ctx, `SELECT doc, version, updated_at FROM profiles WHERE member_id = $1`, member)
}

// Override is the member's override for one namespace; JSON is "{}" when none.
func (s *Store) Override(ctx context.Context, member, namespace string) (Doc, error) {
	return s.doc(ctx, `SELECT doc, version, updated_at FROM profile_overrides WHERE member_id = $1 AND namespace = $2`, member, namespace)
}

func (s *Store) doc(ctx context.Context, sql string, args ...any) (Doc, error) {
	d := Doc{JSON: []byte("{}")}
	if !ValidID(args[0].(string)) {
		return d, nil
	}
	err := s.Pool.QueryRow(ctx, sql, args...).Scan(&d.JSON, &d.Version, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Doc{JSON: []byte("{}")}, nil
	}
	return d, err
}

// IsSupervised: the member has at least one active supervisor.
func (s *Store) IsSupervised(ctx context.Context, member string) (bool, error) {
	if !ValidID(member) {
		return false, nil
	}
	return isSupervised(ctx, s.Pool, member)
}

// PutProfile replaces the member's profile. canonical is profile.Parse's output;
// keys names its top-level fields for the audit row. ifVersion, when not nil,
// must be the stored version (0: none yet).
func (s *Store) PutProfile(ctx context.Context, actor, member string, canonical []byte, keys []string, ifVersion *uint64) (uint64, error) {
	var version uint64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if ok, err := s.isActiveSupervisor(ctx, tx, member, actor); err != nil || !ok {
			return orNotFound(err)
		}
		var cur uint64
		err := tx.QueryRow(ctx, `SELECT version FROM profiles WHERE member_id = $1 FOR UPDATE`, member).Scan(&cur)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if ifVersion != nil && *ifVersion != cur {
			return ErrConflict
		}
		if err := tx.QueryRow(ctx, `INSERT INTO profiles (member_id, doc, updated_by) VALUES ($1, $2, $3)
			ON CONFLICT (member_id) DO UPDATE SET doc = EXCLUDED.doc, version = profiles.version + 1,
			updated_by = EXCLUDED.updated_by, updated_at = now()
			RETURNING version`, member, canonical, actor).Scan(&version); err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, "profile.put", member, "", "", map[string]any{"version": version, "fields": keys}); err != nil {
			return err
		}
		return emit(ctx, tx, "profile", member, "", "", actor)
	})
	return version, err
}

// PutOverride replaces the member's override for one namespace; a nil doc
// deletes it.
func (s *Store) PutOverride(ctx context.Context, actor, member, namespace string, doc []byte) (uint64, error) {
	var version uint64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if ok, err := s.isActiveSupervisor(ctx, tx, member, actor); err != nil || !ok {
			return orNotFound(err)
		}
		action := "override.put"
		if doc == nil {
			action = "override.delete"
			tag, err := tx.Exec(ctx, `DELETE FROM profile_overrides WHERE member_id = $1 AND namespace = $2`, member, namespace)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		} else if err := tx.QueryRow(ctx, `INSERT INTO profile_overrides (member_id, namespace, doc, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (member_id, namespace) DO UPDATE SET doc = EXCLUDED.doc, version = profile_overrides.version + 1,
			updated_by = EXCLUDED.updated_by, updated_at = now()
			RETURNING version`, member, namespace, doc, actor).Scan(&version); err != nil {
			return err
		}
		if err := audit(ctx, tx, actor, action, member, "", namespace, map[string]any{"version": version}); err != nil {
			return err
		}
		return emit(ctx, tx, "override", member, namespace, "", actor)
	})
	return version, err
}
