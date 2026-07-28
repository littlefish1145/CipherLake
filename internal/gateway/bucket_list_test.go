package gateway

import (
	"testing"
	"time"

	"cipherlake/internal/common"
	"cipherlake/internal/metadata"
)

func TestObjectListContentMapsMetadata(t *testing.T) {
	modifiedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	obj := &metadata.ObjectMetadata{
		Key:         "docs/readme.txt",
		ModifiedAt:  modifiedAt,
		ETag:        "etag",
		Size:        42,
		StorageTier: int(common.TierWarm),
	}

	content := objectListContent(obj)

	if content.Key != obj.Key || !content.LastModified.Equal(modifiedAt) || content.ETag != obj.ETag || content.Size != obj.Size {
		t.Fatalf("content = %+v, want object metadata fields", content)
	}
	if content.StorageClass != "WARM" {
		t.Fatalf("storage class = %q, want WARM", content.StorageClass)
	}
	if content.Owner == nil || content.Owner.ID != defaultOwnerID || content.Owner.DisplayName != defaultOwnerID {
		t.Fatalf("owner = %+v, want default owner", content.Owner)
	}
}

func TestCommonPrefixForObject(t *testing.T) {
	got, ok := commonPrefixForObject("docs/2026/readme.txt", "docs/", "/")
	if !ok {
		t.Fatalf("expected common prefix")
	}
	if got != "docs/2026/" {
		t.Fatalf("common prefix = %q, want docs/2026/", got)
	}

	if got, ok := commonPrefixForObject("docs/readme.txt", "docs/", "/"); ok || got != "" {
		t.Fatalf("common prefix = %q/%v, want empty false", got, ok)
	}
}
