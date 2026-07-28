package gateway

import (
	"strings"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"
)

const (
	defaultOwnerID          = "cipherlake-owner"
	defaultOwnerDisplayName = "CipherLake Owner"
)

func defaultObjectOwner() *struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
} {
	return &struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	}{
		ID:          defaultOwnerID,
		DisplayName: defaultOwnerID,
	}
}

func objectListContent(obj *metadata.ObjectMetadata) ListObjectsV2Content {
	return ListObjectsV2Content{
		Key:          obj.Key,
		LastModified: obj.ModifiedAt,
		ETag:         obj.ETag,
		Size:         obj.Size,
		StorageClass: common.StorageTier(obj.StorageTier).String(),
		Owner:        defaultObjectOwner(),
	}
}

func commonPrefixForObject(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	remainder := key[len(prefix):]
	idx := strings.Index(remainder, delimiter)
	if idx < 0 {
		return "", false
	}
	return key[:len(prefix)+idx+len(delimiter)], true
}
