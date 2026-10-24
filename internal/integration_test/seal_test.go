package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/openpak/parental/internal/blobseal"
	"github.com/openpak/parental/internal/store"
)

func blobKey(t *testing.T) *blobseal.Key {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	k, err := blobseal.Parse(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func freshDB(t *testing.T) (string, *pgx.Conn) {
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
	t.Cleanup(func() { conn.Close(ctx) })
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	return url, conn
}

func rawState(t *testing.T, conn *pgx.Conn, ref string) []byte {
	t.Helper()
	var b []byte
	if err := conn.QueryRow(context.Background(), `SELECT state::text FROM devices WHERE device_ref = $1`, ref).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A database from before sealing: the first start with a key seals every blob in place,
// a second start changes nothing, and what an adapter reads back -- the bytes nx-pctl turns
// into what the console is served -- is exactly what it read before.
func TestDeviceBlobsAreSealedAtRest(t *testing.T) {
	url, conn := freshDB(t)
	ctx := context.Background()
	const ref = "00000000000000aa"
	blob := []byte(`{"moonId":170,"unlockCode":"4826","syncedUnlockCode":"8759","setting":{"unlockCode":"4826"},"etag":"\"x\""}`)

	clear, err := store.Open(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	put, err := clear.PutDevice(ctx, parent, store.Device{Namespace: "switch", Ref: ref, Owner: parent, State: blob}, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, err := clear.GetDevice(ctx, "switch", ref)
	if err != nil {
		t.Fatal(err)
	}
	clear.Close()
	if !bytes.Contains(rawState(t, conn, ref), []byte("4826")) {
		t.Fatalf("the clear row does not hold the PIN; the test proves nothing")
	}

	key := blobKey(t)
	st, err := store.Open(ctx, url, key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := rawState(t, conn, ref)
	if bytes.Contains(sealed, []byte("4826")) || bytes.Contains(sealed, []byte("8759")) || !blobseal.IsSealed(sealed) {
		t.Fatalf("after the first start the row is %s", sealed)
	}
	after, err := st.GetDevice(ctx, "switch", ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after.State, before.State) || after.Version != put.Version || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("sealing changed the record:\nbefore %s v%d\nafter  %s v%d", before.State, before.Version, after.State, after.Version)
	}
	st.Close()

	// Idempotent: the next start leaves the sealed row as it is.
	st, err = store.Open(ctx, url, key)
	if err != nil {
		t.Fatal(err)
	}
	if again := rawState(t, conn, ref); !bytes.Equal(again, sealed) {
		t.Fatalf("a second start rewrote the row")
	}

	// A write under the key is sealed, and reads back as Postgres renders the blob, the
	// same bytes a write in clear read back.
	put, err = st.PutDevice(ctx, parent, store.Device{Namespace: "switch", Ref: ref, Owner: parent, State: blob}, put.Version)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawState(t, conn, ref), []byte("4826")) {
		t.Fatalf("a write under the key was stored in clear")
	}
	if !bytes.Equal(put.State, before.State) {
		t.Fatalf("a sealed write reads back %s, a clear one read %s", put.State, before.State)
	}
	var invalid store.InvalidError
	if _, err := st.PutDevice(ctx, parent, store.Device{Namespace: "switch", Ref: ref, Owner: parent,
		State: []byte(`{"sealed":"x"}`)}, put.Version); !errors.As(err, &invalid) {
		t.Fatalf("a blob carrying the reserved member: %v", err)
	}
	st.Close()

	// Sealed rows do not start without their key.
	if s, err := store.Open(ctx, url, nil); err == nil {
		s.Close()
		t.Fatalf("started without a key on sealed rows")
	}
	if s, err := store.Open(ctx, url, blobKey(t)); err == nil {
		s.Close()
		t.Fatalf("started with another key on sealed rows")
	}
}
