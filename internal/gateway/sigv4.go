package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type sigV4Params struct {
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

func parseAuthorizationHeader(auth string) (*sigV4Params, error) {
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return nil, fmt.Errorf("invalid AWS signature version")
	}

	rest := strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 ")
	parts := strings.Split(rest, ", ")

	var credential, signedHeaders, signature string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "Credential=") {
			credential = strings.TrimPrefix(part, "Credential=")
		} else if strings.HasPrefix(part, "SignedHeaders=") {
			signedHeaders = strings.TrimPrefix(part, "SignedHeaders=")
		} else if strings.HasPrefix(part, "Signature=") {
			signature = strings.TrimPrefix(part, "Signature=")
		}
	}

	if credential == "" || signedHeaders == "" || signature == "" {
		return nil, fmt.Errorf("missing required fields in Authorization header")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return nil, fmt.Errorf("invalid credential format, expected accessKey/date/region/service/aws4_request")
	}

	accessKey := credParts[0]
	date := credParts[1]
	region := credParts[2]
	service := credParts[3]
	terminator := credParts[4]
	if terminator != "aws4_request" {
		return nil, fmt.Errorf("invalid credential terminator: %s", terminator)
	}

	headerList := strings.Split(signedHeaders, ";")

	return &sigV4Params{
		AccessKey:     accessKey,
		Date:          date,
		Region:        region,
		Service:       service,
		SignedHeaders: headerList,
		Signature:     signature,
	}, nil
}

func deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

func buildCanonicalRequest(method, path, queryString string, signedHeaders []string, r *http.Request, payloadHash string) (string, error) {
	canonicalURI := getCanonicalURI(path)

	canonicalQueryString := getCanonicalQueryString(queryString)

	var canonicalHeaders strings.Builder
	var signedHeadersList strings.Builder
	for i, h := range signedHeaders {
		if i > 0 {
			signedHeadersList.WriteRune(';')
		}
		signedHeadersList.WriteString(h)

		values := r.Header.Values(h)
		if len(values) == 0 {
			canonicalHeaders.WriteString(h)
			canonicalHeaders.WriteRune(':')
			canonicalHeaders.WriteRune('\n')
			continue
		}

		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteRune(':')
		for j, v := range values {
			if j > 0 {
				canonicalHeaders.WriteRune(',')
			}
			canonicalHeaders.WriteString(strings.TrimSpace(v))
		}
		canonicalHeaders.WriteRune('\n')
	}

	return strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersList.String(),
		payloadHash,
	}, "\n"), nil
}

func getCanonicalURI(path string) string {
	if path == "" {
		path = "/"
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path
	}
	segments := strings.Split(decoded, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	canonical := strings.Join(segments, "/")

	if !strings.HasPrefix(canonical, "/") {
		canonical = "/" + canonical
	}
	return canonical
}

func getCanonicalQueryString(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}

	params := strings.Split(rawQuery, "&")
	type kv struct {
		key   string
		value string
	}
	pairs := make([]kv, 0, len(params))
	for _, p := range params {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		decodedKey, err := url.QueryUnescape(k)
		if err != nil {
			decodedKey = k
		}
		decodedValue, err := url.QueryUnescape(v)
		if err != nil {
			decodedValue = v
		}
		pairs = append(pairs, kv{
			key:   url.QueryEscape(decodedKey),
			value: url.QueryEscape(decodedValue),
		})
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})

	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.key + "=" + p.value
	}
	return strings.Join(encoded, "&")
}

func buildStringToSign(timestamp, date, region, service string, canonicalRequest string) string {
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, region, service)
	canonicalHash := sha256Hash([]byte(canonicalRequest))
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		timestamp,
		scope,
		canonicalHash,
	}, "\n")
}

func computeSignature(signingKey []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
}

func getPayloadHash(r *http.Request) string {
	if v := r.Header.Get("x-amz-content-sha256"); v != "" {
		if v == "UNSIGNED-PAYLOAD" {
			return "UNSIGNED-PAYLOAD"
		}
		return v
	}
	if v := r.URL.Query().Get("X-Amz-Content-SHA256"); v != "" {
		if v == "UNSIGNED-PAYLOAD" {
			return "UNSIGNED-PAYLOAD"
		}
		return v
	}
	return sha256HashStr(nil)
}

func verifySigV4Header(r *http.Request, secretKey string) error {
	auth := r.Header.Get("Authorization")
	params, err := parseAuthorizationHeader(auth)
	if err != nil {
		return err
	}

	timestamp := r.Header.Get("x-amz-date")
	if timestamp == "" {
		timestamp = r.Header.Get("Date")
	}
	if timestamp == "" {
		return fmt.Errorf("missing x-amz-date or Date header")
	}

	if err := validateTimestamp(timestamp, params.Date); err != nil {
		return err
	}

	payloadHash := getPayloadHash(r)

	canonicalRequest, err := buildCanonicalRequest(
		r.Method,
		r.URL.Path,
		r.URL.RawQuery,
		params.SignedHeaders,
		r,
		payloadHash,
	)
	if err != nil {
		return fmt.Errorf("failed to build canonical request: %w", err)
	}

	stringToSign := buildStringToSign(timestamp, params.Date, params.Region, params.Service, canonicalRequest)

	signingKey := deriveSigningKey(secretKey, params.Date, params.Region, params.Service)

	computed := computeSignature(signingKey, stringToSign)

	if subtleConstantTimeCompare(computed, params.Signature) != 1 {
		return fmt.Errorf("signature mismatch")
	}

	return nil
}

func verifySigV4PresignedURL(r *http.Request, secretKey string) error {
	q := r.URL.Query()

	credential := q.Get("X-Amz-Credential")
	if credential == "" {
		return fmt.Errorf("missing X-Amz-Credential in presigned URL")
	}

	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 {
		return fmt.Errorf("invalid X-Amz-Credential format")
	}

	date := credParts[1]
	region := credParts[2]
	service := credParts[3]
	terminator := credParts[4]
	if terminator != "aws4_request" {
		return fmt.Errorf("invalid credential terminator: %s", terminator)
	}

	signedHeadersStr := q.Get("X-Amz-SignedHeaders")
	if signedHeadersStr == "" {
		return fmt.Errorf("missing X-Amz-SignedHeaders in presigned URL")
	}
	signedHeaders := strings.Split(signedHeadersStr, ";")

	signature := q.Get("X-Amz-Signature")
	if signature == "" {
		return fmt.Errorf("missing X-Amz-Signature in presigned URL")
	}

	timestamp := q.Get("X-Amz-Date")
	if timestamp == "" {
		return fmt.Errorf("missing X-Amz-Date in presigned URL")
	}

	expiresStr := q.Get("X-Amz-Expires")
	if expiresStr != "" {
		if err := validatePresignedExpiry(timestamp, expiresStr); err != nil {
			return err
		}
	}

	if err := validateTimestamp(timestamp, date); err != nil {
		return err
	}

	excludedParams := map[string]bool{
		"X-Amz-Signature": true,
	}

	filteredQuery := filterQueryParams(q, excludedParams)
	canonicalQueryString := getCanonicalQueryString(filteredQuery)

	payloadHash := getPayloadHash(r)

	canonicalRequest, err := buildCanonicalRequest(
		r.Method,
		r.URL.Path,
		canonicalQueryString,
		signedHeaders,
		r,
		payloadHash,
	)
	if err != nil {
		return fmt.Errorf("failed to build canonical request: %w", err)
	}

	stringToSign := buildStringToSign(timestamp, date, region, service, canonicalRequest)

	signingKey := deriveSigningKey(secretKey, date, region, service)

	computed := computeSignature(signingKey, stringToSign)

	if subtleConstantTimeCompare(computed, signature) != 1 {
		return fmt.Errorf("presigned URL signature mismatch")
	}

	return nil
}

func filterQueryParams(q url.Values, excluded map[string]bool) string {
	var pairs []string
	for key, values := range q {
		if excluded[key] {
			continue
		}
		for _, v := range values {
			pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func validateTimestamp(timestamp, date string) error {
	t, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		t, err = time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return fmt.Errorf("invalid timestamp format: %s", timestamp)
		}
	}

	now := time.Now().UTC()
	diff := now.Sub(t)
	if diff < -15*time.Minute || diff > 15*time.Minute {
		return fmt.Errorf("request timestamp too far from current time")
	}

	dateFromTimestamp := t.Format("20060102")
	if dateFromTimestamp != date {
		return fmt.Errorf("date in credential does not match timestamp date")
	}

	return nil
}

func validatePresignedExpiry(timestamp, expiresStr string) error {
	t, err := time.Parse("20060102T150405Z", timestamp)
	if err != nil {
		t, err = time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return fmt.Errorf("invalid timestamp format: %s", timestamp)
		}
	}

	var expires time.Duration
	if v, err := time.ParseDuration(expiresStr + "s"); err == nil {
		expires = v
	} else {
		return fmt.Errorf("invalid X-Amz-Expires: %s", expiresStr)
	}

	if time.Now().UTC().After(t.Add(expires)) {
		return fmt.Errorf("presigned URL has expired")
	}

	return nil
}

func hmacSHA256(key []byte, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hash(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func sha256HashStr(data []byte) string {
	return sha256Hash(data)
}

func subtleConstantTimeCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}
