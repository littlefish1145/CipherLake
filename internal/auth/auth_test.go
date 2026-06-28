package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentity_HasPermission(t *testing.T) {
	id := &Identity{Permissions: []string{ActionRead, ActionWrite}}
	assert.True(t, id.HasPermission(ActionRead))
	assert.False(t, id.HasPermission(ActionAdmin))
}

func TestIdentity_HasBucketPermission(t *testing.T) {
	id := &Identity{BucketPerms: map[string][]string{"bucket-a": {ActionRead, ActionWrite}}}
	assert.True(t, id.HasBucketPermission("bucket-a", ActionWrite))
	assert.False(t, id.HasBucketPermission("bucket-a", ActionDelete))
	assert.False(t, id.HasBucketPermission("bucket-b", ActionRead))
}
