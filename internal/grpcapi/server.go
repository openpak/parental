// Package grpcapi is the parental core's gRPC surface for network adapters.
// Every call presents an adapter key in x-internal-key; the key names the
// adapter's namespace (as in voice), and every device call is scoped to it, so
// one network can never read or change another's devices.
package grpcapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/openpak/parental/internal/profile"
	"github.com/openpak/parental/internal/store"
	parentalv1 "github.com/openpak/parental/proto/openpak/parental/v1"
)

// Keys maps adapter namespaces to their keys.
type Keys map[string]string

// ParseKeys reads "switch=key,wiiu=key". Empty keys and bad namespaces are
// errors, so a typo cannot open an unauthenticated adapter.
func ParseKeys(s string) (Keys, error) {
	out := Keys{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		ns, key, ok := strings.Cut(kv, "=")
		ns, key = strings.TrimSpace(ns), strings.TrimSpace(key)
		if !ok || !profile.ValidNamespace(ns) || len(key) < 32 {
			return nil, errors.New("parental: adapter keys are namespace=key (namespace [a-z0-9-], key of at least 32 characters)")
		}
		if _, dup := out[ns]; dup {
			return nil, errors.New("parental: duplicate adapter namespace " + ns)
		}
		out[ns] = key
	}
	if len(out) == 0 {
		return nil, errors.New("parental: no adapter keys configured")
	}
	return out, nil
}

type Server struct {
	parentalv1.UnimplementedParentalServer
	Store *store.Store
	Keys  Keys
}

// adapter returns the namespace the call's key belongs to.
func (s *Server) adapter(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("x-internal-key")
	if len(vals) == 0 || vals[0] == "" {
		return "", status.Error(codes.Unauthenticated, "missing adapter credentials")
	}
	found := ""
	for ns, key := range s.Keys { // every key is compared: no early exit on a match
		if subtle.ConstantTimeCompare([]byte(vals[0]), []byte(key)) == 1 {
			found = ns
		}
	}
	if found == "" {
		return "", status.Error(codes.Unauthenticated, "missing adapter credentials")
	}
	return found, nil
}

type nsKey struct{}

func ns(ctx context.Context) string { v, _ := ctx.Value(nsKey{}).(string); return v }

func (s *Server) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		n, err := s.adapter(ctx)
		if err != nil {
			return nil, err
		}
		return h(context.WithValue(ctx, nsKey{}, n), req)
	}
}

func (s *Server) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		n, err := s.adapter(ss.Context())
		if err != nil {
			return err
		}
		return h(srv, &nsStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), nsKey{}, n)})
	}
}

type nsStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (n *nsStream) Context() context.Context { return n.ctx }

func mapErr(err error) error {
	var inv store.InvalidError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &inv):
		return status.Error(codes.InvalidArgument, inv.Msg)
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		return status.Error(codes.Aborted, "version conflict")
	case errors.Is(err, store.ErrSubjectChange):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Internal, "parental service error")
}

func origin(ctx context.Context) string { return "network:" + ns(ctx) }

func pbSupervisions(in []store.Supervision, member bool, account string) []*parentalv1.Supervision {
	out := []*parentalv1.Supervision{}
	for _, v := range in {
		if (v.MemberID == account) == member {
			out = append(out, &parentalv1.Supervision{MemberId: v.MemberID, SupervisorId: v.SupervisorID, Role: v.Role, State: v.State})
		}
	}
	return out
}

func (s *Server) GetSupervision(ctx context.Context, req *parentalv1.GetSupervisionRequest) (*parentalv1.GetSupervisionResponse, error) {
	acct := strings.ToLower(req.GetAccountId())
	if !store.ValidID(acct) {
		return nil, status.Error(codes.InvalidArgument, "account_id must be an account id")
	}
	rows, err := s.Store.Active(ctx, acct)
	if err != nil {
		return nil, mapErr(err)
	}
	return &parentalv1.GetSupervisionResponse{SupervisedBy: pbSupervisions(rows, true, acct), Supervising: pbSupervisions(rows, false, acct)}, nil
}

func (s *Server) GetProfile(ctx context.Context, req *parentalv1.GetProfileRequest) (*parentalv1.GetProfileResponse, error) {
	m := strings.ToLower(req.GetMemberId())
	if !store.ValidID(m) {
		return nil, status.Error(codes.InvalidArgument, "member_id must be an account id")
	}
	sup, err := s.Store.IsSupervised(ctx, m)
	if err != nil {
		return nil, mapErr(err)
	}
	p, err := s.Store.Profile(ctx, m)
	if err != nil {
		return nil, mapErr(err)
	}
	o, err := s.Store.Override(ctx, m, ns(ctx))
	if err != nil {
		return nil, mapErr(err)
	}
	return &parentalv1.GetProfileResponse{Supervised: sup, ProfileJson: string(p.JSON), ProfileVersion: p.Version,
		OverrideJson: string(o.JSON), OverrideVersion: o.Version}, nil
}

func pbDevice(d store.Device) *parentalv1.Device {
	return &parentalv1.Device{Id: d.ID, DeviceRef: d.Ref, OwnerId: d.Owner, SupervisorIds: d.Supervisors, Subject: d.Subject,
		PairingCode: d.PairingCode, PairingExpiresUnix: d.PairingExpires, PairedAtUnix: d.PairedAt, StateJson: d.State,
		Version: d.Version, UpdatedAtUnix: d.UpdatedAt}
}

func (s *Server) PutDevice(ctx context.Context, req *parentalv1.PutDeviceRequest) (*parentalv1.PutDeviceResponse, error) {
	in := req.GetDevice()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "device is required")
	}
	d, err := s.Store.PutDevice(ctx, origin(ctx), store.Device{Namespace: ns(ctx), Ref: in.GetDeviceRef(), Owner: in.GetOwnerId(),
		Supervisors: in.GetSupervisorIds(), Subject: in.GetSubject(), PairingCode: in.GetPairingCode(),
		PairingExpires: in.GetPairingExpiresUnix(), PairedAt: in.GetPairedAtUnix(), State: in.GetStateJson()}, req.GetExpectedVersion())
	if err != nil {
		return nil, mapErr(err)
	}
	return &parentalv1.PutDeviceResponse{Device: pbDevice(d)}, nil
}

func (s *Server) GetDevice(ctx context.Context, req *parentalv1.GetDeviceRequest) (*parentalv1.Device, error) {
	d, err := s.Store.GetDevice(ctx, ns(ctx), req.GetDeviceRef())
	if err != nil {
		return nil, mapErr(err)
	}
	return pbDevice(d), nil
}

func (s *Server) FindDevice(ctx context.Context, req *parentalv1.FindDeviceRequest) (*parentalv1.Device, error) {
	var d store.Device
	var err error
	switch by := req.GetBy().(type) {
	case *parentalv1.FindDeviceRequest_Subject:
		d, err = s.Store.FindBySubject(ctx, ns(ctx), by.Subject)
	case *parentalv1.FindDeviceRequest_PairingCode:
		d, err = s.Store.FindByPairingCode(ctx, ns(ctx), by.PairingCode)
	default:
		return nil, status.Error(codes.InvalidArgument, "subject or pairing_code is required")
	}
	if err != nil {
		return nil, mapErr(err)
	}
	return pbDevice(d), nil
}

func (s *Server) ListDevices(ctx context.Context, req *parentalv1.ListDevicesRequest) (*parentalv1.ListDevicesResponse, error) {
	if sup := req.GetSupervisorId(); sup != "" && !store.ValidID(sup) {
		return nil, status.Error(codes.InvalidArgument, "supervisor_id must be an account id")
	}
	ds, next, err := s.Store.ListDevices(ctx, ns(ctx), strings.ToLower(req.GetSupervisorId()), req.GetPageToken())
	if err != nil {
		return nil, mapErr(err)
	}
	out := &parentalv1.ListDevicesResponse{NextPageToken: next}
	for _, d := range ds {
		out.Devices = append(out.Devices, pbDevice(d))
	}
	return out, nil
}

func (s *Server) DeleteDevice(ctx context.Context, req *parentalv1.DeleteDeviceRequest) (*parentalv1.DeleteDeviceResponse, error) {
	if err := s.Store.DeleteDevice(ctx, origin(ctx), ns(ctx), req.GetDeviceRef(), req.GetExpectedVersion()); err != nil {
		return nil, mapErr(err)
	}
	return &parentalv1.DeleteDeviceResponse{}, nil
}

func (s *Server) IngestUsage(ctx context.Context, req *parentalv1.IngestUsageRequest) (*parentalv1.IngestUsageResponse, error) {
	evs := make([]store.UsageEvent, 0, len(req.GetEvents()))
	for _, e := range req.GetEvents() {
		evs = append(evs, store.UsageEvent{Payload: e.GetPayloadJson(), Kind: e.GetKind(), OccurredAt: e.GetOccurredAtUnix(), MemberID: strings.ToLower(e.GetMemberId())})
	}
	n, held, err := s.Store.IngestUsage(ctx, ns(ctx), req.GetDeviceRef(), evs)
	if err != nil {
		return nil, mapErr(err)
	}
	return &parentalv1.IngestUsageResponse{Accepted: int32(n), Held: int32(held)}, nil
}

func (s *Server) ListUsage(ctx context.Context, req *parentalv1.ListUsageRequest) (*parentalv1.ListUsageResponse, error) {
	evs, err := s.Store.ListUsage(ctx, ns(ctx), req.GetDeviceRef(), req.GetSinceUnix(), int(req.GetLimit()))
	if err != nil {
		return nil, mapErr(err)
	}
	out := &parentalv1.ListUsageResponse{}
	for _, e := range evs {
		out.Events = append(out.Events, &parentalv1.UsageEvent{PayloadJson: e.Payload, Kind: e.Kind, OccurredAtUnix: e.OccurredAt, MemberId: e.MemberID})
	}
	return out, nil
}

func pbEvent(e store.Event) *parentalv1.ParentalEvent {
	return &parentalv1.ParentalEvent{Version: e.Version, EventId: e.EventID, Type: e.Type, Scope: e.Scope, AccountId: e.AccountID,
		Namespace: e.Namespace, SubjectId: e.SubjectID, Origin: e.Origin, CreatedAtUnix: e.CreatedAt}
}

func (s *Server) PollEvents(ctx context.Context, req *parentalv1.PollEventsRequest) (*parentalv1.PollEventsResponse, error) {
	evs, max, err := s.Store.Poll(ctx, req.GetSinceVersion(), int(req.GetLimit()))
	if err != nil {
		return nil, mapErr(err)
	}
	out := &parentalv1.PollEventsResponse{MaxVersion: max}
	for _, e := range evs {
		out.Events = append(out.Events, pbEvent(e))
	}
	return out, nil
}

func (s *Server) SubscribeEvents(req *parentalv1.SubscribeEventsRequest, stream parentalv1.Parental_SubscribeEventsServer) error {
	err := s.Store.Subscribe(stream.Context(), req.GetSinceVersion(), func(e store.Event) error { return stream.Send(pbEvent(e)) })
	if stream.Context().Err() != nil {
		return nil
	}
	return mapErr(err)
}
