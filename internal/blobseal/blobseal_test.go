package blobseal

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func key(t *testing.T) *Key {
	t.Helper()
	raw := make([]byte, 32)
	rand.Read(raw)
	k, err := Parse(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestARoundTripIsByteExact(t *testing.T) {
	k := key(t)
	plain := []byte(`{"moonId": 1, "unlockCode": "4826", "etag": "\"abc\""}`)
	sealed := k.Seal("switch", "0000000000000001", plain)
	if bytes.Contains(sealed, []byte("4826")) || !IsSealed(sealed) {
		t.Fatalf("sealed blob %s", sealed)
	}
	got, err := Open(k, "switch", "0000000000000001", sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %q, %v", got, err)
	}
	if again := k.Seal("switch", "0000000000000001", plain); bytes.Equal(again, sealed) {
		t.Fatalf("two seals share a nonce")
	}
}

func TestASealedBlobOpensOnlyWhereItWasSealed(t *testing.T) {
	k, other := key(t), key(t)
	sealed := k.Seal("switch", "0000000000000001", []byte(`{}`))
	if _, err := Open(k, "switch", "0000000000000002", sealed); err == nil {
		t.Fatalf("a blob moved to another device opened")
	}
	if _, err := Open(k, "wiiu", "0000000000000001", sealed); err == nil {
		t.Fatalf("a blob moved to another namespace opened")
	}
	if _, err := Open(other, "switch", "0000000000000001", sealed); err == nil || !strings.Contains(err.Error(), k.ID) {
		t.Fatalf("another key: %v", err)
	}
	if _, err := Open(nil, "switch", "0000000000000001", sealed); err == nil {
		t.Fatalf("opened without a key")
	}
}

func TestAClearBlobPassesThrough(t *testing.T) {
	plain := []byte(`{"unlockCode": "4826"}`)
	if IsSealed(plain) {
		t.Fatalf("a clear blob reads as sealed")
	}
	if got, err := Open(nil, "switch", "r", plain); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("clear: %q, %v", got, err)
	}
}

func TestOnlyA32ByteKeyIsTaken(t *testing.T) {
	for _, v := range []string{"", "c2hvcnQ=", base64.StdEncoding.EncodeToString(make([]byte, 31)), "not base64!"} {
		if _, err := Parse(v); err == nil {
			t.Fatalf("%q was taken", v)
		}
	}
	a, _ := Parse(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	b, _ := Parse(base64.StdEncoding.EncodeToString(append(make([]byte, 31), 1)))
	if a.ID == b.ID || len(a.ID) != 16 {
		t.Fatalf("key ids %q %q", a.ID, b.ID)
	}
}
