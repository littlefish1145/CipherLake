package gateway

import (
	"crypto/subtle"
	"net/http"

	"cipherlake/internal/metadata"
	"cipherlake/internal/s3"
	"cipherlake/internal/services"
)

const (
	missingSSECKeyMessage  = "Object encrypted with SSE-C requires customer key headers"
	mismatchSSECKeyMessage = "The provided SSE-C key does not match the key used to encrypt the object"
)

func (s *ObjectService) validateObjectSSECKey(w http.ResponseWriter, r *http.Request, objMeta *metadata.ObjectMetadata, invalidKeyMessage string) ([]byte, bool) {
	clientKey, ssecErr := s3.ParseSSECHeaders(r)
	if ssecErr != nil {
		s.gateway.writeError(w, http.StatusBadRequest, "InvalidRequest", invalidKeyMessage)
		return nil, false
	}
	if clientKey == nil {
		s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", missingSSECKeyMessage)
		return nil, false
	}

	keySHA256 := services.ComputeSSECKeySHA256(clientKey)
	if subtle.ConstantTimeCompare([]byte(keySHA256), []byte(objMeta.SSECKeySHA256)) != 1 {
		zeroClientKey(clientKey)
		s.gateway.writeError(w, http.StatusForbidden, "AccessDenied", mismatchSSECKeyMessage)
		return nil, false
	}

	return clientKey, true
}

func zeroClientKey(clientKey []byte) {
	for i := range clientKey {
		clientKey[i] = 0
	}
}

func objectSSECAlgorithm(ssecUsed bool) string {
	if ssecUsed {
		return "AES256"
	}
	return ""
}
