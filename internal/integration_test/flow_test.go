// The parental core against a real Postgres (PARENTAL_TEST_DATABASE_URL) and a
// stand-in account core: whoami over HTTP, Identity and Events over bufconn.
// Skipped without a database, like every integration surface here. All ids and
// names are made up.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"github.com/openpak/parental/internal/core"
	"github.com/openpak/parental/internal/grpcapi"
	"github.com/openpak/parental/internal/store"
	"github.com/openpak/parental/internal/web"
	parentalv1 "github.com/openpak/parental/proto/openpak/parental/v1"
)

const (
	parent   = "11111111-1111-4111-8111-111111111111"
	member   = "22222222-2222-4222-8222-222222222222"
	guardian = "33333333-3333-4333-8333-333333333333"
	stranger = "44444444-4444-4444-8444-444444444444"
	banned   = "55555555-5555-4555-8555-555555555555"
	kid      = "77777777-7777-4777-8777-777777777777"

	webKey    = "web-key-0123456789abcdef0123456789abcdef"
	switchKey = "switch-key-0123456789abcdef0123456789abcdef"
	wiiuKey   = "wiiu-key-0123456789abcdef0123456789abcdef0"
)

// fakeCore is the account core: tokens are "tok-<account>", every listed account
// is active except banned, and events are whatever the test feeds the stream.
type fakeCore struct {
	accountv1.UnimplementedIdentityServer
	accountv1.UnimplementedEventsServer
	feed chan *accountv1.InvalidationEvent
}

func (f *fakeCore) GetAccount(ctx context.Context, req *accountv1.GetAccountRequest) (*accountv1.GetAccountResponse, error) {
	if md, _ := metadata.FromIncomingContext(ctx); len(md.Get("x-internal-key")) == 0 {
		return nil, status.Error(codes.Unauthenticated, "key")
	}
	switch req.GetAccountId() {
	case parent, member, guardian, stranger:
		return &accountv1.GetAccountResponse{AccountId: req.GetAccountId(), Status: accountv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, Birthday: "1990-01-01"}, nil
	case kid:
		return &accountv1.GetAccountResponse{AccountId: kid, Status: accountv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, Birthday: time.Now().AddDate(-8, 0, 0).Format("2006-01-02")}, nil
	case banned:
		return &accountv1.GetAccountResponse{AccountId: banned, Status: accountv1.AccountStatus_ACCOUNT_STATUS_BANNED}, nil
	}
	return nil, status.Error(codes.NotFound, "no account")
}

func (f *fakeCore) SubscribeAccountEvents(_ *accountv1.SubscribeAccountEventsRequest, s accountv1.Events_SubscribeAccountEventsServer) error {
	for {
		select {
		case <-s.Context().Done():
			return nil
		case ev := <-f.feed:
			if err := s.Send(ev); err != nil {
				return err
			}
		}
	}
}

func whoami(w http.ResponseWriter, r *http.Request) {
	acct, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
	st := "active"
	if acct == banned {
		st = "banned"
	}
	if !store.ValidID(acct) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"account_id": acct, "status": st, "display_name": "made up"})
}

type env struct {
	st     *store.Store
	http   *httptest.Server
	sw, wu parentalv1.ParentalClient
	none   parentalv1.ParentalClient
	core   *fakeCore
	cc     *core.Client
}

func bufconnClient(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	conn, err := grpc.NewClient("passthrough://test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// keyed adds an adapter key to every call on the connection.
func keyed(conn *grpc.ClientConn, key string) parentalv1.ParentalClient {
	return parentalv1.NewParentalClient(&keyConn{conn, key})
}

type keyConn struct {
	*grpc.ClientConn
	key string
}

func (k *keyConn) Invoke(ctx context.Context, m string, a, r any, o ...grpc.CallOption) error {
	if k.key != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-internal-key", k.key)
	}
	return k.ClientConn.Invoke(ctx, m, a, r, o...)
}

func (k *keyConn) NewStream(ctx context.Context, d *grpc.StreamDesc, m string, o ...grpc.CallOption) (grpc.ClientStream, error) {
	if k.key != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-internal-key", k.key)
	}
	return k.ClientConn.NewStream(ctx, d, m, o...)
}

func setup(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("PARENTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PARENTAL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	st, err := store.Open(ctx, url, blobKey(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	// The account core.
	fc := &fakeCore{feed: make(chan *accountv1.InvalidationEvent, 8)}
	coreLis := bufconn.Listen(1 << 20)
	coreSrv := grpc.NewServer()
	accountv1.RegisterIdentityServer(coreSrv, fc)
	accountv1.RegisterEventsServer(coreSrv, fc)
	go coreSrv.Serve(coreLis)
	t.Cleanup(coreSrv.Stop)
	coreHTTP := httptest.NewServer(http.HandlerFunc(whoami))
	t.Cleanup(coreHTTP.Close)
	cconn := bufconnClient(t, coreLis)
	cc := &core.Client{Identity: accountv1.NewIdentityClient(cconn), Events: accountv1.NewEventsClient(cconn),
		Key: "core-key", HTTPURL: coreHTTP.URL, HTTP: coreHTTP.Client()}

	// This service.
	keys, err := grpcapi.ParseKeys("switch=" + switchKey + ",wiiu=" + wiiuKey)
	if err != nil {
		t.Fatal(err)
	}
	api := &grpcapi.Server{Store: st, Keys: keys}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(api.Unary()), grpc.StreamInterceptor(api.Stream()))
	parentalv1.RegisterParentalServer(gs, api)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	pconn := bufconnClient(t, lis)

	hs := httptest.NewServer((&web.Server{Store: st, Core: cc, Key: webKey}).Handler())
	t.Cleanup(hs.Close)
	return &env{st: st, http: hs, sw: keyed(pconn, switchKey), wu: keyed(pconn, wiiuKey), none: keyed(pconn, ""), core: fc, cc: cc}
}

// do calls the HTTP surface as account (empty: no bearer) and decodes the answer.
func (e *env) do(t *testing.T, account, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	req.Header.Set("X-Internal-Key", webKey)
	if account != "" {
		req.Header.Set("Authorization", "Bearer tok-"+account)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func want(t *testing.T, what string, got, wantCode int, body map[string]any) {
	t.Helper()
	if got != wantCode {
		t.Fatalf("%s: %d %v, want %d", what, got, body, wantCode)
	}
}

func TestHTTPAuthentication(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("GET", e.http.URL+"/v1/me/supervision", nil)
	req.Header.Set("Authorization", "Bearer tok-"+parent)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("no internal key: %d", resp.StatusCode)
	}
	code, body := e.do(t, "", "GET", "/v1/me/supervision", nil)
	want(t, "no bearer", code, 401, body)
	code, body = e.do(t, "not-a-session", "GET", "/v1/me/supervision", nil)
	want(t, "bad bearer", code, 401, body)
	code, body = e.do(t, banned, "GET", "/v1/me/supervision", nil)
	want(t, "banned session", code, 401, body)
	code, body = e.do(t, parent, "GET", "/v1/me/supervision", nil)
	want(t, "signed in", code, 200, body)
	if resp, _ := http.Get(e.http.URL + "/healthz"); resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
}

func TestSupervisionAndProfile(t *testing.T) {
	e := setup(t)
	mp := "/v1/me/members/" + member

	code, body := e.do(t, parent, "POST", "/v1/me/members", map[string]any{"member_id": parent})
	want(t, "self", code, 400, body)
	code, body = e.do(t, parent, "POST", "/v1/me/members", map[string]any{"member_id": "66666666-6666-4666-8666-666666666666"})
	want(t, "unknown account", code, 404, body)
	code, body = e.do(t, parent, "POST", "/v1/me/members", map[string]any{"member_id": member})
	want(t, "invite", code, 201, body)
	code, body = e.do(t, stranger, "POST", "/v1/me/members", map[string]any{"member_id": member})
	want(t, "second owner", code, 409, body)

	// Pending grants nothing.
	code, body = e.do(t, parent, "GET", mp+"/profile", nil)
	want(t, "pending owner reads", code, 404, body)
	// Only the member accepts an owner invite.
	code, body = e.do(t, parent, "POST", "/v1/me/supervisions/"+member+"/"+parent+"/accept", nil)
	want(t, "owner accepts own invite", code, 404, body)
	code, body = e.do(t, member, "POST", "/v1/me/supervisions/"+member+"/"+parent+"/accept", nil)
	want(t, "member accepts", code, 204, body)

	// A supervised account cannot supervise.
	code, body = e.do(t, member, "POST", "/v1/me/members", map[string]any{"member_id": stranger})
	want(t, "member supervising", code, 400, body)

	// Guardians: only the owner invites, only the guardian accepts.
	code, body = e.do(t, stranger, "POST", mp+"/guardians", map[string]any{"account_id": guardian})
	want(t, "stranger adds guardian", code, 404, body)
	code, body = e.do(t, parent, "POST", mp+"/guardians", map[string]any{"account_id": banned})
	want(t, "banned guardian", code, 404, body)
	code, body = e.do(t, parent, "POST", mp+"/guardians", map[string]any{"account_id": guardian})
	want(t, "add guardian", code, 201, body)
	code, body = e.do(t, member, "POST", "/v1/me/supervisions/"+member+"/"+guardian+"/accept", nil)
	want(t, "member accepts for the guardian", code, 404, body)
	code, body = e.do(t, guardian, "POST", "/v1/me/supervisions/"+member+"/"+guardian+"/accept", nil)
	want(t, "guardian accepts", code, 204, body)

	code, body = e.do(t, member, "GET", "/v1/me/supervision", nil)
	if code != 200 || len(body["supervised_by"].([]any)) != 2 {
		t.Fatalf("member's view %d %v", code, body)
	}

	// Profiles.
	prof := map[string]any{"rating_age": 12, "play_time": map[string]any{"sat": 180}, "communication": map[string]any{"mode": "friends_only"}}
	code, body = e.do(t, guardian, "PUT", mp+"/profile", map[string]any{"profile": prof})
	want(t, "guardian sets profile", code, 200, body)
	code, body = e.do(t, member, "PUT", mp+"/profile", map[string]any{"profile": map[string]any{}})
	want(t, "member lifts own limits", code, 404, body)
	code, body = e.do(t, stranger, "GET", mp+"/profile", nil)
	want(t, "stranger reads", code, 404, body)
	code, body = e.do(t, parent, "PUT", mp+"/profile", map[string]any{"profile": map[string]any{"rating_age": 30}})
	if code != 400 || !strings.Contains(body["error"].(string), "rating_age") {
		t.Fatalf("invalid profile %d %v", code, body)
	}
	code, body = e.do(t, parent, "PUT", mp+"/profile", map[string]any{"profile": prof, "version": 0})
	want(t, "stale version", code, 409, body)
	code, body = e.do(t, member, "GET", mp+"/profile", nil)
	if code != 200 || body["version"].(float64) != 1 || body["profile"].(map[string]any)["rating_age"].(float64) != 12 {
		t.Fatalf("member reads %d %v", code, body)
	}

	// Overrides.
	code, body = e.do(t, parent, "PUT", mp+"/overrides/switch", map[string]any{"doc": map[string]any{"restrictionMode": "ALARM"}})
	want(t, "override", code, 200, body)
	code, body = e.do(t, parent, "PUT", mp+"/overrides/Bad_NS", map[string]any{"doc": map[string]any{}})
	want(t, "bad namespace", code, 400, body)
	gp, err := e.sw.GetProfile(context.Background(), &parentalv1.GetProfileRequest{MemberId: member})
	if err != nil || !gp.Supervised || !strings.Contains(gp.OverrideJson, "ALARM") || gp.ProfileVersion != 1 {
		t.Fatalf("switch reads %v %v", gp, err)
	}
	if gp, _ := e.wu.GetProfile(context.Background(), &parentalv1.GetProfileRequest{MemberId: member}); gp.OverrideJson != "{}" {
		t.Fatalf("wiiu sees switch's override: %v", gp)
	}
	code, body = e.do(t, parent, "DELETE", mp+"/overrides/switch", nil)
	want(t, "delete override", code, 204, body)

	// The audit trail names fields, never values.
	code, body = e.do(t, parent, "GET", mp+"/audit", nil)
	raw, _ := json.Marshal(body)
	if code != 200 || !strings.Contains(string(raw), "profile.put") || strings.Contains(string(raw), "ALARM") || strings.Contains(string(raw), "friends_only") {
		t.Fatalf("audit %d %s", code, raw)
	}
	code, body = e.do(t, member, "GET", mp+"/audit", nil)
	want(t, "member reads audit", code, 404, body)

	// Events: a member-scoped profile event, written by the guardian.
	evs, err := e.sw.PollEvents(context.Background(), &parentalv1.PollEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs.Events {
		if ev.Scope == "profile" && ev.AccountId == member && ev.Origin == guardian && ev.Type == "parental_changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no profile event in %v", evs.Events)
	}

	// Leaving: the member cannot; a guardian can; the owner leaving ends it all.
	code, body = e.do(t, member, "DELETE", "/v1/me/supervisions/"+member+"/"+parent, nil)
	want(t, "member removes owner", code, 404, body)
	code, body = e.do(t, guardian, "DELETE", "/v1/me/supervisions/"+member+"/"+guardian, nil)
	want(t, "guardian leaves", code, 204, body)
	code, body = e.do(t, parent, "DELETE", "/v1/me/supervisions/"+member+"/"+parent, nil)
	want(t, "owner ends", code, 204, body)
	if gp, _ := e.sw.GetProfile(context.Background(), &parentalv1.GetProfileRequest{MemberId: member}); gp.Supervised || gp.ProfileVersion != 0 {
		t.Fatalf("profile outlived the supervision: %v", gp)
	}
}

func TestDevicesAndUsage(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	if _, err := e.none.GetDevice(ctx, &parentalv1.GetDeviceRequest{DeviceRef: "x"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: %v", err)
	}

	exp := time.Now().Add(15 * time.Minute).Unix()
	put, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: &parentalv1.Device{
		DeviceRef: "0000a1b2c3d4e5f6", OwnerId: parent, SupervisorIds: []string{guardian},
		PairingCode: "123456", PairingExpiresUnix: exp, StateJson: []byte(`{"unlockCode":"0000","moonId":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	d := put.Device
	if d.Version != 1 || len(d.SupervisorIds) != 2 || d.SupervisorIds[0] != parent {
		t.Fatalf("created %v", d)
	}
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: d}); status.Code(err) != codes.Aborted {
		t.Fatalf("create over an existing record: %v", err)
	}
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: &parentalv1.Device{DeviceRef: "bad ref", OwnerId: parent}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad ref: %v", err)
	}
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: &parentalv1.Device{DeviceRef: "r", OwnerId: "acct-1"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad owner: %v", err)
	}

	// The code finds the record while live, in this namespace only.
	if f, err := e.sw.FindDevice(ctx, &parentalv1.FindDeviceRequest{By: &parentalv1.FindDeviceRequest_PairingCode{PairingCode: "123456"}}); err != nil || f.DeviceRef != d.DeviceRef {
		t.Fatalf("find by code %v %v", f, err)
	}
	if _, err := e.wu.FindDevice(ctx, &parentalv1.FindDeviceRequest{By: &parentalv1.FindDeviceRequest_PairingCode{PairingCode: "123456"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("another network found it: %v", err)
	}
	if _, err := e.wu.GetDevice(ctx, &parentalv1.GetDeviceRequest{DeviceRef: d.DeviceRef}); status.Code(err) != codes.NotFound {
		t.Fatalf("another network read it: %v", err)
	}

	// Paired: the code is burnt, the subject bound for good.
	d.Subject, d.PairingCode, d.PairingExpiresUnix, d.PairedAtUnix = "cert-0000000000000001", "", 0, time.Now().Unix()
	put, err = e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: d, ExpectedVersion: 1})
	if err != nil || put.Device.Version != 2 {
		t.Fatalf("pair %v %v", put, err)
	}
	if _, err := e.sw.FindDevice(ctx, &parentalv1.FindDeviceRequest{By: &parentalv1.FindDeviceRequest_PairingCode{PairingCode: "123456"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("a burnt code still finds: %v", err)
	}
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: d, ExpectedVersion: 1}); status.Code(err) != codes.Aborted {
		t.Fatalf("stale version: %v", err)
	}
	d.Subject = "cert-0000000000000002"
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: d, ExpectedVersion: 2}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rebinding: %v", err)
	}
	d.Subject = "cert-0000000000000001"
	if f, err := e.sw.FindDevice(ctx, &parentalv1.FindDeviceRequest{By: &parentalv1.FindDeviceRequest_Subject{Subject: d.Subject}}); err != nil || f.Version != 2 {
		t.Fatalf("find by subject %v %v", f, err)
	}

	// Usage: kept byte for byte, deduplicated.
	ev1 := []byte(`{"eventType":"DID_APP_PLAY", "eventTimestamp": 1}`)
	ev2 := []byte(`{"eventType":"DID_WRONG_UNLOCK_CODE","eventTimestamp":` + jsonInt(time.Now().Unix()) + `}`)
	in, err := e.sw.IngestUsage(ctx, &parentalv1.IngestUsageRequest{DeviceRef: d.DeviceRef, Events: []*parentalv1.UsageEvent{
		{PayloadJson: ev1, Kind: "DID_APP_PLAY"},
		{PayloadJson: ev2, Kind: "DID_WRONG_UNLOCK_CODE", OccurredAtUnix: time.Now().Unix()},
	}})
	if err != nil || in.Accepted != 2 || in.Held != 2 {
		t.Fatalf("ingest %v %v", in, err)
	}
	if in, _ := e.sw.IngestUsage(ctx, &parentalv1.IngestUsageRequest{DeviceRef: d.DeviceRef, Events: []*parentalv1.UsageEvent{{PayloadJson: ev1}}}); in.Accepted != 0 || in.Held != 2 {
		t.Fatalf("a retried upload was counted again: %v", in)
	}
	if _, err := e.sw.IngestUsage(ctx, &parentalv1.IngestUsageRequest{DeviceRef: d.DeviceRef, Events: []*parentalv1.UsageEvent{{PayloadJson: []byte("{")}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("broken payload: %v", err)
	}
	lu, err := e.sw.ListUsage(ctx, &parentalv1.ListUsageRequest{DeviceRef: d.DeviceRef})
	if err != nil || len(lu.Events) != 2 || !bytes.Equal(lu.Events[0].PayloadJson, ev1) {
		t.Fatalf("list usage %v %v", lu, err)
	}

	// The parent view: supervisors see the device and its usage, nobody else.
	code, body := e.do(t, guardian, "GET", "/v1/me/devices", nil)
	devs, _ := body["devices"].([]any)
	if code != 200 || len(devs) != 1 {
		t.Fatalf("guardian's devices %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "unlockCode") || strings.Contains(string(raw), "123456") {
		t.Fatalf("the device list leaks the blob or the code: %s", raw)
	}
	id := devs[0].(map[string]any)["id"].(string)
	code, body = e.do(t, parent, "GET", "/v1/me/devices/"+id+"/usage?days=7", nil)
	if code != 200 || len(body["events"].([]any)) != 2 { // an event with no time is never filtered out
		t.Fatalf("usage for the week %d %v", code, body)
	}
	code, body = e.do(t, stranger, "GET", "/v1/me/devices/"+id+"/usage", nil)
	want(t, "stranger's usage", code, 404, body)

	// Listing pages the namespace, filtered by supervisor.
	ls, err := e.sw.ListDevices(ctx, &parentalv1.ListDevicesRequest{SupervisorId: guardian})
	if err != nil || len(ls.Devices) != 1 {
		t.Fatalf("list %v %v", ls, err)
	}
	if ls, _ := e.sw.ListDevices(ctx, &parentalv1.ListDevicesRequest{SupervisorId: stranger}); len(ls.Devices) != 0 {
		t.Fatalf("stranger's list %v", ls)
	}

	// Deleting needs the current version, and takes usage with it.
	if _, err := e.sw.DeleteDevice(ctx, &parentalv1.DeleteDeviceRequest{DeviceRef: d.DeviceRef, ExpectedVersion: 1}); status.Code(err) != codes.Aborted {
		t.Fatalf("stale delete: %v", err)
	}
	if _, err := e.sw.DeleteDevice(ctx, &parentalv1.DeleteDeviceRequest{DeviceRef: d.DeviceRef, ExpectedVersion: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sw.ListUsage(ctx, &parentalv1.ListUsageRequest{DeviceRef: d.DeviceRef}); status.Code(err) != codes.NotFound {
		t.Fatalf("usage outlived the device: %v", err)
	}
	var n int
	e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d usage rows left", n)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSubscribeSeesNewEvents(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := e.sw.SubscribeEvents(ctx, &parentalv1.SubscribeEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: &parentalv1.Device{DeviceRef: "0000000000000abc", OwnerId: parent}}); err != nil {
		t.Fatal(err)
	}
	ev, err := stream.Recv()
	if err != nil || ev.Scope != "device" || ev.Namespace != "switch" || ev.SubjectId != "0000000000000abc" || ev.Origin != "network:switch" || ev.AccountId != parent {
		t.Fatalf("event %v %v", ev, err)
	}
}

func TestDeletedAccountIsPurged(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.do(t, parent, "POST", "/v1/me/members", map[string]any{"member_id": member})
	e.do(t, member, "POST", "/v1/me/supervisions/"+member+"/"+parent+"/accept", nil)
	e.do(t, parent, "PUT", "/v1/me/members/"+member+"/profile", map[string]any{"profile": map[string]any{"rating_age": 7}})
	if _, err := e.sw.PutDevice(ctx, &parentalv1.PutDeviceRequest{Device: &parentalv1.Device{DeviceRef: "0000000000000def", OwnerId: parent}}); err != nil {
		t.Fatal(err)
	}

	go e.cc.Follow(ctx, e.st)
	e.core.feed <- &accountv1.InvalidationEvent{Version: 41, Type: "presence_changed", AccountId: member}
	e.core.feed <- &accountv1.InvalidationEvent{Version: 42, Type: "account_deleted", AccountId: parent}

	deadline := time.Now().Add(5 * time.Second)
	for {
		cur, _ := e.st.CoreCursor(ctx)
		if cur == 42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor stuck at %d", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for table, q := range map[string]string{
		"supervisions":                  `SELECT count(*) FROM supervisions`,
		"devices":                       `SELECT count(*) FROM devices`,
		"audit by or about the account": `SELECT count(*) FROM audit_log WHERE actor = '` + parent + `'`,
	} {
		var n int
		if err := e.st.Pool.QueryRow(ctx, q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d left (%v)", table, n, err)
		}
	}
	// The owner going ends the member's supervision, restrictions included, as
	// when an owner leaves.
	if p, _ := e.st.Profile(ctx, member); p.Version != 0 {
		t.Errorf("member's profile outlived its owner: version %d", p.Version)
	}
}

// Decision 2 (2026-10-23): an under-13 member needs no acceptance; an adult
// supervisor may leave at any time.
func TestUnder13NeedsNoAcceptanceAndGuardianMayLeave(t *testing.T) {
	e := setup(t)
	code, body := e.do(t, parent, "POST", "/v1/me/members", map[string]any{"member_id": kid})
	want(t, "link under-13", code, 201, body)
	if body["state"] != "active" {
		t.Fatalf("under-13 link state %v, want active", body["state"])
	}
	code, body = e.do(t, parent, "GET", "/v1/me/members/"+kid+"/profile", nil)
	if code == 404 {
		t.Fatalf("supervisor of an under-13 member cannot read: %d %v", code, body)
	}
	code, body = e.do(t, parent, "POST", "/v1/me/members/"+kid+"/guardians", map[string]any{"account_id": guardian})
	want(t, "add guardian", code, 201, body)
	code, body = e.do(t, guardian, "POST", "/v1/me/supervisions/"+kid+"/"+guardian+"/accept", nil)
	want(t, "guardian accepts", code, 204, body)
	code, body = e.do(t, guardian, "DELETE", "/v1/me/supervisions/"+kid+"/"+guardian, nil)
	want(t, "guardian leaves", code, 204, body)
	code, body = e.do(t, parent, "DELETE", "/v1/me/supervisions/"+kid+"/"+parent, nil)
	want(t, "owner leaves", code, 204, body)
}
