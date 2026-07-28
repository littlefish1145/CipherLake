package gateway

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
)

type objectChecksumRecorder struct {
	provided     string
	checksumType string
	sha256       hash.Hash
	crc32c       hash.Hash
	md5          hash.Hash
	writer       io.Writer
}

type objectChecksumMismatch struct {
	checksumType string
	expected     string
	actual       string
}

func (e *objectChecksumMismatch) Error() string {
	return fmt.Sprintf("%s checksum mismatch: expected %s, got %s", e.checksumType, e.expected, e.actual)
}

func (e *objectChecksumMismatch) clientMessage() string {
	return e.Error()
}

func newObjectChecksumRecorder(header http.Header) *objectChecksumRecorder {
	recorder := &objectChecksumRecorder{
		sha256: sha256.New(),
	}

	if c := header.Get("x-amz-checksum-crc32c"); c != "" {
		recorder.provided = c
		recorder.checksumType = "crc32c"
		recorder.crc32c = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	} else if c := header.Get("x-amz-checksum-sha256"); c != "" {
		recorder.provided = c
		recorder.checksumType = "sha256"
	} else if c := header.Get("x-amz-checksum-md5"); c != "" {
		recorder.provided = c
		recorder.checksumType = "md5"
		recorder.md5 = md5.New()
	} else if c := header.Get("Content-MD5"); c != "" {
		recorder.provided = c
		recorder.checksumType = "md5"
		recorder.md5 = md5.New()
	}

	writers := []io.Writer{recorder.sha256}
	if recorder.crc32c != nil {
		writers = append(writers, recorder.crc32c)
	}
	if recorder.md5 != nil {
		writers = append(writers, recorder.md5)
	}
	recorder.writer = io.MultiWriter(writers...)

	return recorder
}

func (r *objectChecksumRecorder) Writer() io.Writer {
	return r.writer
}

func (r *objectChecksumRecorder) Finalize() (string, string, error) {
	computedSHA256 := base64.StdEncoding.EncodeToString(r.sha256.Sum(nil))

	var storedChecksum string
	var storedChecksumType string

	switch r.checksumType {
	case "crc32c":
		storedChecksum = base64.StdEncoding.EncodeToString(r.crc32c.Sum(nil))
		storedChecksumType = "crc32c"
	case "sha256":
		storedChecksum = computedSHA256
		storedChecksumType = "sha256"
	case "md5":
		storedChecksum = base64.StdEncoding.EncodeToString(r.md5.Sum(nil))
		storedChecksumType = "md5"
	default:
		return computedSHA256, "sha256", nil
	}

	if r.provided != storedChecksum {
		return "", "", &objectChecksumMismatch{
			checksumType: strings.ToUpper(storedChecksumType),
			expected:     r.provided,
			actual:       storedChecksum,
		}
	}

	return storedChecksum, storedChecksumType, nil
}

func parseUserMetadata(header http.Header) map[string]string {
	metadataMap := make(map[string]string)
	for k, values := range header {
		if len(values) == 0 {
			continue
		}
		lowerKey := strings.ToLower(k)
		if strings.HasPrefix(lowerKey, "x-amz-meta-") {
			metadataMap[strings.TrimPrefix(lowerKey, "x-amz-meta-")] = values[0]
		}
	}
	return metadataMap
}
