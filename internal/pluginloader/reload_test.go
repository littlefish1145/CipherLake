package pluginloader

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrDependencyUnavailable(t *testing.T) {
	e := ErrDependencyUnavailable{Plugin: "demo"}
	require.Contains(t, e.Error(), "demo")
	require.Contains(t, e.Error(), "reload")
	require.True(t, IsErrDependencyUnavailable(e))
	require.False(t, IsErrDependencyUnavailable(errors.New("other")))
}

func TestIsErrHasDependents(t *testing.T) {
	e := ErrHasDependents{Plugin: "base", Dependents: []string{"dep"}}
	require.True(t, IsErrHasDependents(e))
	require.False(t, IsErrHasDependents(errors.New("other")))
}
