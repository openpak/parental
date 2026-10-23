// Package web is the parental core's HTTP surface for the website and the app
// (docs/design.md section 3). Every route but /healthz needs the website's
// internal key AND the user's account-core session as a bearer; the session is
// resolved by the core's own whoami, and the caller is always that account.
// A member or device the caller may not see answers 404, never 403.
package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openpak/parental/internal/core"
	"github.com/openpak/parental/internal/profile"
	"github.com/openpak/parental/internal/store"
)

// Identity is what the routes need from the account core.
type Identity interface {
	Whoami(ctx context.Context, bearer string) (string, error)
	Active(ctx context.Context, account string) (bool, error)
}

type Server struct {
	Store *store.Store
	Core  Identity
	Key   string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	a := func(pattern string, h func(http.ResponseWriter, *http.Request, string)) {
		mux.HandleFunc(pattern, s.auth(h))
	}
	a("GET /v1/me/supervision", s.mine)
	a("POST /v1/me/members", s.invite)
	a("POST /v1/me/members/{member}/guardians", s.addGuardian)
	a("POST /v1/me/supervisions/{member}/{supervisor}/accept", s.accept)
	a("DELETE /v1/me/supervisions/{member}/{supervisor}", s.remove)
	a("GET /v1/me/members/{member}/profile", s.getProfile)
	a("PUT /v1/me/members/{member}/profile", s.putProfile)
	a("PUT /v1/me/members/{member}/overrides/{namespace}", s.putOverride)
	a("DELETE /v1/me/members/{member}/overrides/{namespace}", s.deleteOverride)
	a("GET /v1/me/members/{member}/audit", s.audit)
	a("GET /v1/me/devices", s.devices)
	a("GET /v1/me/devices/{id}/usage", s.usage)
	return mux
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Key == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Internal-Key")), []byte(s.Key)) != 1 {
			out(w, http.StatusUnauthorized, map[string]any{"error": "internal key"})
			return
		}
		bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || bearer == "" {
			out(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		account, err := s.Core.Whoami(r.Context(), bearer)
		if errors.Is(err, core.ErrUnauthenticated) {
			out(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		if err != nil {
			log.Printf("parental: whoami: %v", err)
			out(w, http.StatusBadGateway, map[string]any{"error": "account service unavailable"})
			return
		}
		h(w, r, strings.ToLower(account))
	}
}

func out(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	var inv store.InvalidError
	switch {
	case errors.As(err, &inv):
		out(w, http.StatusBadRequest, map[string]any{"error": inv.Msg})
	case errors.Is(err, store.ErrNotFound):
		out(w, http.StatusNotFound, map[string]any{"error": "not found"})
	case errors.Is(err, store.ErrConflict):
		out(w, http.StatusConflict, map[string]any{"error": "conflict"})
	default:
		log.Printf("parental: %v", err)
		out(w, http.StatusInternalServerError, map[string]any{"error": "parental service error"})
	}
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v); err != nil {
		out(w, http.StatusBadRequest, map[string]any{"error": "body is not the expected JSON object"})
		return false
	}
	return true
}

func (s *Server) mine(w http.ResponseWriter, r *http.Request, me string) {
	rows, err := s.Store.Mine(r.Context(), me)
	if err != nil {
		fail(w, err)
		return
	}
	by, of := []store.Supervision{}, []store.Supervision{}
	for _, v := range rows {
		if v.MemberID == me {
			by = append(by, v)
		} else {
			of = append(of, v)
		}
	}
	out(w, 200, map[string]any{"account_id": me, "supervised_by": by, "supervising": of})
}

// activeAccount refuses an id the account core does not know as active, the same
// 404 as any id the caller may not see.
func (s *Server) activeAccount(w http.ResponseWriter, r *http.Request, id string) bool {
	if !store.ValidID(id) {
		out(w, http.StatusBadRequest, map[string]any{"error": "an account id is required"})
		return false
	}
	ok, err := s.Core.Active(r.Context(), id)
	if err != nil {
		log.Printf("parental: account lookup: %v", err)
		out(w, http.StatusBadGateway, map[string]any{"error": "account service unavailable"})
		return false
	}
	if !ok {
		out(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
	return ok
}

func (s *Server) invite(w http.ResponseWriter, r *http.Request, me string) {
	var in struct {
		MemberID string `json:"member_id"`
	}
	if !decode(w, r, 4096, &in) {
		return
	}
	member := strings.ToLower(in.MemberID)
	if member == me {
		out(w, http.StatusBadRequest, map[string]any{"error": "an account cannot supervise itself"})
		return
	}
	if !s.activeAccount(w, r, member) {
		return
	}
	if err := s.Store.Invite(r.Context(), me, member); err != nil {
		fail(w, err)
		return
	}
	out(w, http.StatusCreated, map[string]any{"member_id": member, "supervisor_id": me, "role": "owner", "state": "pending"})
}

func (s *Server) addGuardian(w http.ResponseWriter, r *http.Request, me string) {
	var in struct {
		AccountID string `json:"account_id"`
	}
	if !decode(w, r, 4096, &in) {
		return
	}
	member, guardian := strings.ToLower(r.PathValue("member")), strings.ToLower(in.AccountID)
	if ok, err := s.Store.CanView(r.Context(), me, member); err != nil || !ok || member == me {
		fail(w, orNotFound(err))
		return
	}
	if !s.activeAccount(w, r, guardian) {
		return
	}
	if err := s.Store.AddGuardian(r.Context(), me, member, guardian); err != nil {
		fail(w, err)
		return
	}
	out(w, http.StatusCreated, map[string]any{"member_id": member, "supervisor_id": guardian, "role": "guardian", "state": "pending"})
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return store.ErrNotFound
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request, me string) {
	if err := s.Store.Accept(r.Context(), me, strings.ToLower(r.PathValue("member")), strings.ToLower(r.PathValue("supervisor"))); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, me string) {
	if err := s.Store.Remove(r.Context(), me, strings.ToLower(r.PathValue("member")), strings.ToLower(r.PathValue("supervisor"))); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request, me string) {
	member := strings.ToLower(r.PathValue("member"))
	if ok, err := s.Store.CanView(r.Context(), me, member); err != nil || !ok {
		fail(w, orNotFound(err))
		return
	}
	p, err := s.Store.Profile(r.Context(), member)
	if err != nil {
		fail(w, err)
		return
	}
	out(w, 200, map[string]any{"member_id": member, "profile": json.RawMessage(p.JSON), "version": p.Version})
}

func (s *Server) putProfile(w http.ResponseWriter, r *http.Request, me string) {
	var in struct {
		Profile json.RawMessage `json:"profile"`
		Version *uint64         `json:"version"`
	}
	if !decode(w, r, 256<<10, &in) {
		return
	}
	if len(in.Profile) == 0 {
		out(w, http.StatusBadRequest, map[string]any{"error": "profile is required"})
		return
	}
	p, canon, err := profile.Parse(in.Profile)
	if err != nil {
		out(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	member := strings.ToLower(r.PathValue("member"))
	v, err := s.Store.PutProfile(r.Context(), me, member, canon, fields(p), in.Version)
	if err != nil {
		fail(w, err)
		return
	}
	out(w, 200, map[string]any{"member_id": member, "version": v})
}

// fields names a profile's set top-level fields, for the audit row.
func fields(p *profile.Profile) []string {
	var m map[string]json.RawMessage
	b, _ := json.Marshal(p)
	_ = json.Unmarshal(b, &m)
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) putOverride(w http.ResponseWriter, r *http.Request, me string) {
	ns := r.PathValue("namespace")
	if !profile.ValidNamespace(ns) {
		out(w, http.StatusBadRequest, map[string]any{"error": "namespace must match [a-z0-9-]{1,32}"})
		return
	}
	var in struct {
		Doc json.RawMessage `json:"doc"`
	}
	if !decode(w, r, profile.MaxOverrideBytes+1024, &in) {
		return
	}
	doc, err := profile.ParseOverride(in.Doc)
	if err != nil {
		out(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	v, err := s.Store.PutOverride(r.Context(), me, strings.ToLower(r.PathValue("member")), ns, doc)
	if err != nil {
		fail(w, err)
		return
	}
	out(w, 200, map[string]any{"namespace": ns, "version": v})
}

func (s *Server) deleteOverride(w http.ResponseWriter, r *http.Request, me string) {
	if _, err := s.Store.PutOverride(r.Context(), me, strings.ToLower(r.PathValue("member")), r.PathValue("namespace"), nil); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request, me string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Store.Audit(r.Context(), me, strings.ToLower(r.PathValue("member")), limit)
	if err != nil {
		fail(w, err)
		return
	}
	out(w, 200, map[string]any{"entries": rows})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request, me string) {
	ds, err := s.Store.SupervisedDevices(r.Context(), me)
	if err != nil {
		fail(w, err)
		return
	}
	out(w, 200, map[string]any{"devices": ds})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, me string) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > store.UsageDays {
			out(w, http.StatusBadRequest, map[string]any{"error": "days must be 1 to 30"})
			return
		}
		days = n
	}
	since := time.Now().AddDate(0, 0, -days).Unix()
	evs, err := s.Store.DeviceUsage(r.Context(), me, strings.ToLower(r.PathValue("id")), since)
	if err != nil {
		fail(w, err)
		return
	}
	if evs == nil {
		evs = []store.UsageEvent{}
	}
	out(w, 200, map[string]any{"events": evs})
}
