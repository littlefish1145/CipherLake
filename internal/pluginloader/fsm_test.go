package pluginloader

import (
	"encoding/json"
	"errors"
	"testing"

	craft "cipherlake/internal/raft"
	hashicraft "github.com/hashicorp/raft"
	"github.com/stretchr/testify/require"
)

func TestLoaderFSM_ApplyUnknownOp(t *testing.T) {
	fsm, err := NewLoaderFSMFromDir(t.TempDir())
	require.NoError(t, err)
	defer fsm.Close()

	op := &LoaderFSMOp{Type: "unknown_op", Plugin: "x"}
	data, _ := json.Marshal(op)
	result := fsm.Apply(&hashicraft.Log{Data: data})
	res, ok := result.(*craft.FSMApplyResult)
	require.True(t, ok)
	require.False(t, res.Success)
	require.Contains(t, res.Error, "unknown op")
}

func TestLoaderFSM_ApplyHookRegisterUnregister(t *testing.T) {
	fsm, err := NewLoaderFSMFromDir(t.TempDir())
	require.NoError(t, err)
	defer fsm.Close()

	rec := &HookRecord{PluginName: "p", HookID: "p-0", Operation: "s3:GetObject"}
	recJSON, _ := json.Marshal(rec)

	op := &LoaderFSMOp{Type: OpHookRegister, Plugin: "p", Key: "p-0", Data: recJSON}
	data, _ := json.Marshal(op)
	result := fsm.Apply(&hashicraft.Log{Data: data})
	res := result.(*craft.FSMApplyResult)
	require.True(t, res.Success)

	op = &LoaderFSMOp{Type: OpHookUnregister, Plugin: "p", Key: "p-0"}
	data, _ = json.Marshal(op)
	result = fsm.Apply(&hashicraft.Log{Data: data})
	res = result.(*craft.FSMApplyResult)
	require.True(t, res.Success)
}

func TestLoaderFSM_ApplyUnmarshalError(t *testing.T) {
	fsm, err := NewLoaderFSMFromDir(t.TempDir())
	require.NoError(t, err)
	defer fsm.Close()

	result := fsm.Apply(&hashicraft.Log{Data: []byte("not json")})
	res := result.(*craft.FSMApplyResult)
	require.False(t, res.Success)
	require.Contains(t, res.Error, "unmarshal")
}

func TestIsErrVersionMismatch(t *testing.T) {
	require.True(t, IsErrVersionMismatch(ErrVersionMismatch{}))
	require.False(t, IsErrVersionMismatch(nil))
	require.False(t, IsErrVersionMismatch(errors.New("other")))
}
