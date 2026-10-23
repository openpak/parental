// Package core is the parental service's client of the account core, for three
// things only: who a web session belongs to (whoami), whether an account exists
// and is active (invites), and the account lifecycle (account_deleted purges).
// Nothing here changes the account core.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	accountv1 "github.com/openpak/parental/internal/accountpb"
)

// ErrUnauthenticated is a session the core does not accept, or an account that
// is not active.
var ErrUnauthenticated = errors.New("core: not signed in")

type Client struct {
	Identity accountv1.IdentityClient
	Events   accountv1.EventsClient
	Key      string
	// HTTPURL is the core's HTTP base (its /v1/whoami); HTTP is the client for it.
	HTTPURL string
	HTTP    *http.Client
}

// Dial opens the gRPC connection. Insecure on purpose: the internal network is
// the boundary, as for every other internal caller of the core.
func Dial(grpcAddr, httpURL, key string) (*Client, error) {
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("core: dial: %w", err)
	}
	return &Client{
		Identity: accountv1.NewIdentityClient(conn),
		Events:   accountv1.NewEventsClient(conn),
		Key:      key,
		HTTPURL:  httpURL,
		HTTP:     &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (c *Client) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-internal-key", c.Key)
}

// Whoami resolves a web session through the core's own GET /v1/whoami. Only an
// active account is signed in here. The token is never logged.
func (c *Client) Whoami(ctx context.Context, bearer string) (string, error) {
	if bearer == "" {
		return "", ErrUnauthenticated
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.HTTPURL+"/v1/whoami", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("core: whoami: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", ErrUnauthenticated
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("core: whoami answered %d", resp.StatusCode)
	}
	var who struct {
		AccountID string `json:"account_id"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&who); err != nil {
		return "", fmt.Errorf("core: whoami: %w", err)
	}
	if who.AccountID == "" || who.Status != "active" {
		return "", ErrUnauthenticated
	}
	return who.AccountID, nil
}

// Active reports whether the account exists and is active.
func (c *Client) Active(ctx context.Context, account string) (bool, error) {
	rctx, cancel := context.WithTimeout(c.ctx(ctx), 5*time.Second)
	defer cancel()
	resp, err := c.Identity.GetAccount(rctx, &accountv1.GetAccountRequest{AccountId: account})
	if status.Code(err) == codes.NotFound || status.Code(err) == codes.InvalidArgument {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return resp.GetStatus() == accountv1.AccountStatus_ACCOUNT_STATUS_ACTIVE, nil
}

// Cursor is where the follower is in the core's stream, kept durably.
type Cursor interface {
	CoreCursor(ctx context.Context) (uint64, error)
	SetCoreCursor(ctx context.Context, v uint64) error
	PurgeAccount(ctx context.Context, account string) error
}

// Follow consumes the core's event stream until ctx ends, purging every account
// the core reports deleted and saving the cursor after each event it handled.
// A broken stream is resumed from the saved cursor after a pause.
func (c *Client) Follow(ctx context.Context, st Cursor) {
	for ctx.Err() == nil {
		err := c.followOnce(ctx, st)
		if ctx.Err() != nil {
			return
		}
		log.Printf("parental: account core stream: %v; resuming in 5s", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Client) followOnce(ctx context.Context, st Cursor) error {
	since, err := st.CoreCursor(ctx)
	if err != nil {
		return err
	}
	stream, err := c.Events.SubscribeAccountEvents(c.ctx(ctx), &accountv1.SubscribeAccountEventsRequest{SinceVersion: since})
	if err != nil {
		return err
	}
	// Most of the stream is presence and friends, which this service ignores: the
	// cursor is saved after every purge and otherwise every few seconds. Purging
	// is idempotent, so re-reading a few events after a restart costs nothing.
	saved := time.Now()
	for {
		ev, err := stream.Recv()
		if err != nil {
			return err
		}
		purge := ev.GetType() == "account_deleted"
		if purge {
			if err := st.PurgeAccount(ctx, ev.GetAccountId()); err != nil {
				return fmt.Errorf("purge after event %d: %w", ev.GetVersion(), err)
			}
			log.Printf("parental: purged a deleted account's parental data (core event %d)", ev.GetVersion())
		}
		if purge || time.Since(saved) > 5*time.Second {
			if err := st.SetCoreCursor(ctx, ev.GetVersion()); err != nil {
				return err
			}
			saved = time.Now()
		}
	}
}
