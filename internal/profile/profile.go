// Package profile is the neutral restriction profile (docs/design.md section 2)
// and the one validator every surface goes through. A document is refused, not
// thinned: a parent who set a limit must never be shown a save that dropped it.
package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Profile v1. Every field is optional; absent means "not restricted".
type Profile struct {
	RatingAge     *int           `json:"rating_age,omitempty"`
	PlayTime      map[string]int `json:"play_time,omitempty"`
	Bedtime       *Bedtime       `json:"bedtime,omitempty"`
	Titles        *Titles        `json:"titles,omitempty"`
	Communication *Communication `json:"communication,omitempty"`
	Store         *Store         `json:"store,omitempty"`
}

type Bedtime struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Title struct {
	Namespace string `json:"namespace"`
	TitleID   string `json:"title_id"`
}

type Titles struct {
	Allow []Title `json:"allow,omitempty"`
	Block []Title `json:"block,omitempty"`
}

type Communication struct {
	Mode                    string  `json:"mode,omitempty"`
	FriendRequests          string  `json:"friend_requests,omitempty"`
	UGCPosting              *bool   `json:"ugc_posting,omitempty"`
	FreeCommunication       string  `json:"free_communication,omitempty"`
	FreeCommunicationTitles []Title `json:"free_communication_titles,omitempty"`
}

type Store struct {
	Purchases     string         `json:"purchases,omitempty"`
	SpendingLimit *SpendingLimit `json:"spending_limit,omitempty"`
}

type SpendingLimit struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Period      string `json:"period"`
}

// MaxTitles bounds each title list.
const MaxTitles = 500

// MaxOverrideBytes bounds a per-network override document.
const MaxOverrideBytes = 64 << 10

var (
	namespaceRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	titleRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	clockRe     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	currencyRe  = regexp.MustCompile(`^[A-Z]{3}$`)
	days        = map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}
)

// ValidNamespace is what a network namespace may look like.
func ValidNamespace(ns string) bool { return namespaceRe.MatchString(ns) }

// Parse decodes and validates a profile document and returns it in canonical
// form (the encoder's key order, absent fields dropped).
func Parse(doc []byte) (*Profile, []byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, nil, fmt.Errorf("profile: %w", err)
	}
	if dec.More() {
		return nil, nil, errors.New("profile: trailing data")
	}
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	out, err := json.Marshal(&p)
	return &p, out, err
}

func bad(path, why string) error { return fmt.Errorf("profile: %s: %s", path, why) }

func oneOf(path, v string, allowed ...string) error {
	if v == "" {
		return nil
	}
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return bad(path, fmt.Sprintf("must be one of %v", allowed))
}

func titles(path string, ts []Title) error {
	if len(ts) > MaxTitles {
		return bad(path, fmt.Sprintf("at most %d titles", MaxTitles))
	}
	for i, t := range ts {
		if !namespaceRe.MatchString(t.Namespace) {
			return bad(fmt.Sprintf("%s[%d].namespace", path, i), "must match [a-z0-9-]{1,32}")
		}
		if !titleRe.MatchString(t.TitleID) {
			return bad(fmt.Sprintf("%s[%d].title_id", path, i), "must match [A-Za-z0-9._-]{1,64}")
		}
	}
	return nil
}

func (p *Profile) validate() error {
	if p.RatingAge != nil && (*p.RatingAge < 0 || *p.RatingAge > 21) {
		return bad("rating_age", "must be 0 to 21")
	}
	for d, m := range p.PlayTime {
		if !days[d] {
			return bad("play_time."+d, "keys are mon..sun")
		}
		if m < 0 || m > 1440 {
			return bad("play_time."+d, "minutes must be 0 to 1440")
		}
	}
	if b := p.Bedtime; b != nil {
		if !clockRe.MatchString(b.Start) {
			return bad("bedtime.start", "must be HH:MM")
		}
		if !clockRe.MatchString(b.End) {
			return bad("bedtime.end", "must be HH:MM")
		}
	}
	if t := p.Titles; t != nil {
		if err := titles("titles.allow", t.Allow); err != nil {
			return err
		}
		if err := titles("titles.block", t.Block); err != nil {
			return err
		}
	}
	if c := p.Communication; c != nil {
		if err := oneOf("communication.mode", c.Mode, "unrestricted", "friends_only", "none"); err != nil {
			return err
		}
		if err := oneOf("communication.friend_requests", c.FriendRequests, "allowed", "blocked"); err != nil {
			return err
		}
		if err := oneOf("communication.free_communication", c.FreeCommunication, "allowed", "restricted"); err != nil {
			return err
		}
		if err := titles("communication.free_communication_titles", c.FreeCommunicationTitles); err != nil {
			return err
		}
	}
	if s := p.Store; s != nil {
		if err := oneOf("store.purchases", s.Purchases, "allowed", "blocked"); err != nil {
			return err
		}
		if l := s.SpendingLimit; l != nil {
			if l.AmountMinor < 0 || l.AmountMinor > 1_000_000_000 {
				return bad("store.spending_limit.amount_minor", "must be 0 to 1000000000")
			}
			if !currencyRe.MatchString(l.Currency) {
				return bad("store.spending_limit.currency", "must be three upper-case letters")
			}
			if err := oneOf("store.spending_limit.period", l.Period, "day", "week", "month"); err != nil || l.Period == "" {
				return bad("store.spending_limit.period", "must be day, week or month")
			}
		}
	}
	return nil
}

// ParseOverride checks a per-network override: a JSON object of bounded size.
// Its contents belong to the network that namespace names.
func ParseOverride(doc []byte) ([]byte, error) {
	if len(doc) > MaxOverrideBytes {
		return nil, fmt.Errorf("override: larger than %d bytes", MaxOverrideBytes)
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil || m == nil {
		return nil, errors.New("override: must be a JSON object")
	}
	return json.Marshal(m)
}
