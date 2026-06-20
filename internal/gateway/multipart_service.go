package gateway

import "net/http"

type MultipartService struct {
	gateway *S3Gateway
	handler *MultipartUploadHandler
}

func NewMultipartService(gateway *S3Gateway) *MultipartService {
	return &MultipartService{
		gateway: gateway,
		handler: NewMultipartUploadHandler(gateway),
	}
}

func (s *MultipartService) HandleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	return s.handler.HandleCreateMultipartUpload(w, r, bucket, key)
}

func (s *MultipartService) HandleUploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	return s.handler.HandleUploadPart(w, r, bucket, key)
}

func (s *MultipartService) HandleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	return s.handler.HandleCompleteMultipartUpload(w, r, bucket, key)
}

func (s *MultipartService) HandleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	return s.handler.HandleAbortMultipartUpload(w, r, bucket, key)
}

func (s *MultipartService) HandleListParts(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	return s.handler.HandleListParts(w, r, bucket, key)
}

func (s *MultipartService) HandleListUploads(w http.ResponseWriter, r *http.Request, bucket string) error {
	return s.handler.HandleListUploads(w, r, bucket)
}
