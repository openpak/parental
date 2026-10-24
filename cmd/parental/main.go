// Parental: the console-neutral parental-controls core (docs/design.md).
// Supervision, restriction profiles, per-network overrides, device pairing
// records, usage reports and the audit trail, keyed by the account core's
// account id. Network adapters (nx-pctl first) call it over gRPC with a key
// bound to their namespace; the website calls its HTTP surface with the user's
// account-core session. Ports (block 20170): 20170 HTTP, 20171 gRPC.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/openpak/parental/internal/blobseal"
	"github.com/openpak/parental/internal/core"
	"github.com/openpak/parental/internal/grpcapi"
	"github.com/openpak/parental/internal/store"
	"github.com/openpak/parental/internal/web"
	parentalv1 "github.com/openpak/parental/proto/openpak/parental/v1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// secret reads NAME, or the file named by NAME_FILE (preferred: nothing secret
// in the unit's environment).
func secret(name string) (string, error) {
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(name), nil
}

// blobKey is PARENTAL_BLOB_KEY (or _FILE), which seals the device blobs at rest: they hold
// console PINs. Without it the service does not start, unless
// PARENTAL_BLOB_INSECURE_PLAINTEXT=1 says blobs may stay in clear (development and tests
// only, never production): then the key is nil.
func blobKey() (*blobseal.Key, error) {
	v, err := secret("PARENTAL_BLOB_KEY")
	if err != nil {
		return nil, err
	}
	if v == "" {
		if os.Getenv("PARENTAL_BLOB_INSECURE_PLAINTEXT") == "1" {
			log.Printf("parental: PARENTAL_BLOB_INSECURE_PLAINTEXT=1: device blobs, console PINs included, are kept in clear (never in production)")
			return nil, nil
		}
		return nil, errors.New("parental: PARENTAL_BLOB_KEY (or _FILE) is required: device blobs hold console PINs (PARENTAL_BLOB_INSECURE_PLAINTEXT=1 for development only)")
	}
	k, err := blobseal.Parse(v)
	if err != nil {
		return nil, fmt.Errorf("parental: PARENTAL_BLOB_KEY: %w", err)
	}
	return k, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dbURL, err := secret("PARENTAL_DATABASE_URL")
	if err != nil || dbURL == "" {
		return fmt.Errorf("parental: PARENTAL_DATABASE_URL (or _FILE) is required: %v", err)
	}
	coreKey, err := secret("PARENTAL_CORE_INTERNAL_KEY")
	if err != nil || coreKey == "" {
		return fmt.Errorf("parental: the account core credential is required (PARENTAL_CORE_INTERNAL_KEY or _FILE): %v", err)
	}
	webKey, err := secret("PARENTAL_INTERNAL_KEY")
	if err != nil || len(webKey) < 32 {
		return fmt.Errorf("parental: PARENTAL_INTERNAL_KEY (or _FILE) of at least 32 characters is required: %v", err)
	}
	raw, err := secret("PARENTAL_ADAPTER_KEYS")
	if err != nil {
		return err
	}
	keys, err := grpcapi.ParseKeys(strings.ReplaceAll(raw, "\n", ","))
	if err != nil {
		return err
	}
	blobKey, err := blobKey()
	if err != nil {
		return err
	}
	retention, err := time.ParseDuration(env("PARENTAL_EVENT_RETENTION", "168h"))
	if err != nil || retention < time.Hour {
		return errors.New("parental: PARENTAL_EVENT_RETENTION must be a Go duration of at least 1h")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	st, err := store.Open(ctx, dbURL, blobKey)
	if err != nil {
		return err
	}
	defer st.Close()

	cc, err := core.Dial(env("PARENTAL_CORE_ADDRESS", "127.0.0.1:20001"), env("PARENTAL_CORE_HTTP_URL", "http://127.0.0.1:20000"), coreKey)
	if err != nil {
		return err
	}
	go cc.Follow(ctx, st)
	go func() {
		for {
			if n, err := st.PruneEvents(ctx, retention); err != nil {
				log.Printf("parental: event prune: %v", err)
			} else if n > 0 {
				log.Printf("parental: pruned %d event(s) older than %s", n, retention)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()

	api := &grpcapi.Server{Store: st, Keys: keys}
	lis, err := net.Listen("tcp", env("PARENTAL_GRPC_ADDR", "127.0.0.1:20171"))
	if err != nil {
		return err
	}
	gs := grpc.NewServer(grpc.UnaryInterceptor(api.Unary()), grpc.StreamInterceptor(api.Stream()))
	parentalv1.RegisterParentalServer(gs, api)
	hs := health.NewServer()
	hs.SetServingStatus("parental", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(gs, hs)
	go func() {
		log.Printf("parental: grpc on %s (adapters: %d)", lis.Addr(), len(keys))
		if err := gs.Serve(lis); err != nil {
			log.Printf("parental: grpc: %v", err)
			stop()
		}
	}()

	srv := &http.Server{
		Addr:              env("PARENTAL_HTTP_ADDR", "127.0.0.1:20170"),
		Handler:           (&web.Server{Store: st, Core: cc, Key: webKey}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		log.Printf("parental: http on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("parental: http: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	// Event subscriptions never end on their own; give unary calls 5 s.
	done := make(chan struct{})
	go func() { gs.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		gs.Stop()
	}
	sd, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sd)
}
