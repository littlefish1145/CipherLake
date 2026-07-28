package pluginloader

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrManifestInvalid_Error(t *testing.T) {
	err := ErrManifestInvalid{Field: "name", Reason: "too short"}
	require.Contains(t, err.Error(), "name")
	require.Contains(t, err.Error(), "too short")
}

func TestIsErrManifestInvalid(t *testing.T) {
	require.True(t, IsErrManifestInvalid(ErrManifestInvalid{}))
	require.False(t, IsErrManifestInvalid(errors.New("other")))
}
