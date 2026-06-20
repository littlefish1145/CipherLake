// Package errors provides domain errors for the Nexus S3 gateway.
// Each error maps to an S3-compatible XML error code and HTTP status.
package errors

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
)

// Error is a domain error that can be rendered as an S3 XML error response.
type Error struct {
	Code      string // S3 error code, e.g. "NoSuchKey"
	Message   string // Human-readable message
	Status    int    // HTTP status code
	Resource  string // Optional resource identifier
	RequestID string // Optional request ID
	cause     error  // Optional underlying error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying error for errors.Is/As inspection.
func (e *Error) Unwrap() error { return e.cause }

// IsS3 reports whether err is a Nexus S3 domain error.
func IsS3(err error) (*Error, bool) {
	var s3Err *Error
	if errors.As(err, &s3Err) {
		return s3Err, true
	}
	return nil, false
}

// New creates a new S3 domain error.
func New(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

// Newf creates a new formatted S3 domain error.
func Newf(status int, code, format string, args ...interface{}) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Status: status}
}

// Wrap wraps an underlying error with an S3 domain error.
func Wrap(err error, code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status, cause: err}
}

// WithResource attaches a resource identifier to the error.
func (e *Error) WithResource(resource string) *Error {
	e.Resource = resource
	return e
}

// WithRequestID attaches a request ID to the error.
func (e *Error) WithRequestID(requestID string) *Error {
	e.RequestID = requestID
	return e
}

// Response renders the error as an S3 XML response payload.
func (e *Error) Response() Response {
	return Response{
		XMLName:   xml.Name{Local: "Error"},
		Code:      e.Code,
		Message:   e.Message,
		Resource:  e.Resource,
		RequestID: e.RequestID,
	}
}

// Response is the S3-compatible XML error envelope.
type Response struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

// Write renders err as an S3 XML error response to w. If err is not an S3
// domain error, it is converted to "InternalError" with status 500.
func Write(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/xml")

	s3Err, ok := IsS3(err)
	if !ok {
		s3Err = InternalError(err)
	}

	if s3Err.RequestID == "" {
		s3Err.RequestID = generateRequestID()
	}
	w.Header().Set("x-amz-request-id", s3Err.RequestID)
	w.WriteHeader(s3Err.Status)

	resp := s3Err.Response()
	_ = xml.NewEncoder(w).Encode(resp)
}

// Common S3 error constructors.
var (
	NoSuchKey = func() *Error {
		return New("NoSuchKey", "The specified key does not exist.", http.StatusNotFound)
	}
	NoSuchBucket = func() *Error {
		return New("NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound)
	}
	NoSuchUpload = func() *Error {
		return New("NoSuchUpload", "The specified upload does not exist.", http.StatusNotFound)
	}
	AccessDenied = func() *Error {
		return New("AccessDenied", "Access Denied.", http.StatusForbidden)
	}
	InvalidRequest = func() *Error {
		return New("InvalidRequest", "Invalid request.", http.StatusBadRequest)
	}
	InvalidBucketName = func() *Error {
		return New("InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest)
	}
	InvalidKey = func() *Error {
		return New("InvalidKey", "The specified key is not valid.", http.StatusBadRequest)
	}
	InvalidURI = func() *Error {
		return New("InvalidURI", "The specified URI is not valid.", http.StatusBadRequest)
	}
	InvalidRange = func() *Error {
		return New("InvalidRange", "The requested range is not satisfiable.", http.StatusRequestedRangeNotSatisfiable)
	}
	EntityTooLarge = func() *Error {
		return New("EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", http.StatusRequestEntityTooLarge)
	}
	PreconditionFailed = func() *Error {
		return New("PreconditionFailed", "At least one of the preconditions you specified did not hold.", http.StatusPreconditionFailed)
	}
	BadDigest = func() *Error {
		return New("BadDigest", "The Content-MD5 you specified did not match what we received.", http.StatusBadRequest)
	}
	ObjectLocked = func() *Error {
		return New("ObjectLocked", "Object is under retention lock.", http.StatusForbidden)
	}
	SlowDown = func() *Error {
		return New("SlowDown", "Please reduce your request rate.", http.StatusTooManyRequests)
	}
	UploadExpired = func() *Error {
		return New("UploadExpired", "Upload session has expired.", http.StatusGone)
	}
	OffsetMismatch = func() *Error {
		return New("OffsetMismatch", "Upload offset does not match.", http.StatusConflict)
	}
)

// InternalError returns an InternalError wrapping err.
func InternalError(err error) *Error {
	return Wrap(err, "InternalError", "An internal error occurred.", http.StatusInternalServerError)
}

func generateRequestID() string {
	// Minimal random-looking request ID; gateway can override with a proper generator.
	return fmt.Sprintf("%016x", http.StatusInternalServerError)
}
