package gateway

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

func TestObjectChecksumRecorderDefaultsToSHA256(t *testing.T) {
	recorder := newObjectChecksumRecorder(http.Header{})

	if _, err := recorder.Writer().Write([]byte("hello")); err != nil {
		t.Fatalf("write checksum bytes: %v", err)
	}

	gotChecksum, gotType, err := recorder.Finalize()
	if err != nil {
		t.Fatalf("finalize checksum: %v", err)
	}

	sum := sha256.Sum256([]byte("hello"))
	wantChecksum := base64.StdEncoding.EncodeToString(sum[:])
	if gotChecksum != wantChecksum {
		t.Fatalf("checksum = %q, want %q", gotChecksum, wantChecksum)
	}
	if gotType != "sha256" {
		t.Fatalf("checksum type = %q, want sha256", gotType)
	}
}

func TestObjectChecksumRecorderValidatesContentMD5(t *testing.T) {
	body := []byte("hello")
	sum := md5.Sum(body)
	md5Base64 := base64.StdEncoding.EncodeToString(sum[:])

	recorder := newObjectChecksumRecorder(http.Header{
		"Content-Md5": {md5Base64},
	})

	if _, err := recorder.Writer().Write(body); err != nil {
		t.Fatalf("write checksum bytes: %v", err)
	}

	gotChecksum, gotType, err := recorder.Finalize()
	if err != nil {
		t.Fatalf("finalize checksum: %v", err)
	}
	if gotChecksum != md5Base64 {
		t.Fatalf("checksum = %q, want %q", gotChecksum, md5Base64)
	}
	if gotType != "md5" {
		t.Fatalf("checksum type = %q, want md5", gotType)
	}
}

func TestObjectChecksumRecorderReturnsMismatch(t *testing.T) {
	recorder := newObjectChecksumRecorder(http.Header{
		"X-Amz-Checksum-Sha256": {"not-the-checksum"},
	})

	if _, err := recorder.Writer().Write([]byte("hello")); err != nil {
		t.Fatalf("write checksum bytes: %v", err)
	}

	_, _, err := recorder.Finalize()
	var mismatch *objectChecksumMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want objectChecksumMismatch", err)
	}
	if got, want := mismatch.clientMessage(), "SHA256 checksum mismatch: expected not-the-checksum, got LPJNul+wow4m6DsqxbninhsWHlwfp0JecwQzYpOLmCQ="; got != want {
		t.Fatalf("client message = %q, want %q", got, want)
	}
}

func TestParseUserMetadataCanonicalizesHeaderKeys(t *testing.T) {
	got := parseUserMetadata(http.Header{
		"X-Amz-Meta-Foo": {"bar"},
		"x-amz-meta-Baz": {"qux"},
	})

	if got["foo"] != "bar" {
		t.Fatalf("metadata foo = %q, want bar", got["foo"])
	}
	if got["baz"] != "qux" {
		t.Fatalf("metadata baz = %q, want qux", got["baz"])
	}
}
