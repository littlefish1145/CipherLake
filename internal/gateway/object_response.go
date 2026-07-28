package gateway

import (
	"net/http"
	"strconv"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"
)

type objectResponseHeaderOptions struct {
	ContentLength       int64
	ETag                string
	AcceptRanges        bool
	CacheControl        string
	CacheStatus         string
	ContentRange        string
	IncludeStorageClass bool
}

func writeObjectResponseHeaders(w http.ResponseWriter, r *http.Request, objMeta *metadata.ObjectMetadata, opts objectResponseHeaderOptions) {
	etag := opts.ETag
	if etag == "" {
		etag = `"` + objMeta.ETag + `"`
	}

	w.Header().Set("Content-Type", objMeta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(opts.ContentLength, 10))
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", objMeta.ModifiedAt.Format(http.TimeFormat))

	if opts.AcceptRanges {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if opts.CacheControl != "" {
		w.Header().Set("Cache-Control", opts.CacheControl)
	}
	if opts.CacheStatus != "" {
		w.Header().Set("X-Cache", opts.CacheStatus)
	}
	if opts.ContentRange != "" {
		w.Header().Set("Content-Range", opts.ContentRange)
	}
	if opts.IncludeStorageClass {
		w.Header().Set("X-Amz-Storage-Class", common.StorageTier(objMeta.StorageTier).String())
	}

	setObjectEncryptionResponseHeaders(w, r, objMeta)

	for k, v := range objMeta.UserMetadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}

	setChecksumResponseHeaders(w, objMeta, r)
}

func setObjectEncryptionResponseHeaders(w http.ResponseWriter, r *http.Request, objMeta *metadata.ObjectMetadata) {
	if objMeta.SSECUsed {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
		if keyMD5 := r.Header.Get("x-amz-server-side-encryption-customer-key-MD5"); keyMD5 != "" {
			w.Header().Set("x-amz-server-side-encryption-customer-key-MD5", keyMD5)
		}
		return
	}

	if objMeta.Encrypted {
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	}
}
