package external

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestJobCursorIsAuthenticatedAndBoundToTenantAndFilter(t *testing.T) {
	codec, err := NewJobCursorCodec([]byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	want := JobCursor{
		TenantID:   "aaaaaaaaaaaaaaaaaaaaaaaaaa",
		Status:     JobStatusQueued,
		CreatedAt:  time.Date(2026, 7, 19, 10, 11, 12, 123000000, time.UTC),
		InternalID: 42,
	}
	encoded, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.Decode(encoded, want.TenantID, want.Status)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded cursor = %+v, want %+v", got, want)
	}

	parts := strings.Split(encoded, ".")
	if len(parts) != 2 {
		t.Fatalf("cursor parts = %d", len(parts))
	}
	tampered := parts[0] + "." + strings.Repeat("A", len(parts[1]))
	if _, err := codec.Decode(tampered, want.TenantID, want.Status); !errors.Is(err, ErrInvalidJobCursor) {
		t.Fatalf("tampered cursor error = %v", err)
	}
	if _, err := codec.Decode(encoded, "bbbbbbbbbbbbbbbbbbbbbbbbbb", want.Status); !errors.Is(err, ErrInvalidJobCursor) {
		t.Fatalf("cross-tenant cursor error = %v", err)
	}
	if _, err := codec.Decode(encoded, want.TenantID, JobStatusRunning); !errors.Is(err, ErrInvalidJobCursor) {
		t.Fatal("wrong tenant/status call unexpectedly succeeded")
	}
}

func TestJobCursorCodecRejectsWeakKeysAndInvalidPositions(t *testing.T) {
	if _, err := NewJobCursorCodec([]byte("weak")); err == nil {
		t.Fatal("weak cursor key accepted")
	}
	codec, err := NewJobCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	for name, cursor := range map[string]JobCursor{
		"tenant": {TenantID: "bad", CreatedAt: time.Now(), InternalID: 1},
		"time":   {TenantID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", InternalID: 1},
		"id":     {TenantID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Now()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codec.Encode(cursor); !errors.Is(err, ErrInvalidJobCursor) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSourceObjectKeyContainsNoCallerControlledPath(t *testing.T) {
	key, err := SourceObjectKey("aaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if key != "external/aaaaaaaaaaaaaaaaaaaaaaaaaa/sources/bbbbbbbbbbbbbbbbbbbbbbbbbb.bin" {
		t.Fatalf("key = %q", key)
	}
	if _, err := SourceObjectKey("../tenant", "bbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("path-like tenant ID accepted")
	}
}

func TestWorkerClaimFormattingRedactsSourceMetadataAndLeaseToken(t *testing.T) {
	claim := WorkerJobClaim{
		Job: ExternalJobRecord{
			ExternalID: "aaaaaaaaaaaaaaaaaaaaaaaaaa",
			Source: SourceObjectMetadata{
				ObjectKey: "external/private/source.bin", SHA256: []byte("private-digest"),
				Nonce: []byte("private-nonce"),
			},
		},
		WorkerID: "worker-a", AttemptNo: 2, LeaseToken: []byte("private-lease-token-32-bytes!!!!!"),
	}
	for _, formatted := range []string{fmt.Sprint(claim), fmt.Sprintf("%+v", claim), fmt.Sprintf("%#v", claim)} {
		for _, secret := range []string{"external/private", "private-digest", "private-nonce", "private-lease"} {
			if strings.Contains(formatted, secret) {
				t.Fatalf("formatted claim leaked %q: %s", secret, formatted)
			}
		}
	}
}
