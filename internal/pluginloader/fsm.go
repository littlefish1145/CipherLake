// Package pluginloader implements the Nexus plugin loader service.
//
// The loader is an independent microservice that hosts WASM plugin instances
// (via wazero) and brokers their access to host capabilities. All persistent
// state is replicated through a Raft FSM (this file) backed by BoltDB.
//
// State layout (BoltDB top-level buckets):
//   - plugin_registry      : installed plugin metadata (name → manifest+meta)
//   - trusted_keys_mirror  : Ed25519 public keys (Raft-replicated copy of
//                            config/trusted_keys.json, kept in sync by the
//                            admin API so all followers see the same set)
//   - plugin_state          : per-plugin state KV (nested bucket per plugin_name)
//   - hook_registry         : registered S3 hooks (plugin_name → hook list)
//
// See the design spec at .trae/specs/plugin-system/spec.txt for the full
// 17 design decisions and 15 audit corrections this code implements.
package pluginloader

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	craft "cipherlake/internal/raft"
	hashicraft "github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
)

// Top-level BoltDB buckets managed by the LoaderFSM.
const (
	BucketPluginRegistry = "plugin_registry"
	BucketTrustedKeys     = "trusted_keys_mirror"
	BucketPluginState     = "plugin_state"   // nested bucket per plugin_name
	BucketHookRegistry    = "hook_registry"
)

// LoaderFSM is a Raft FSM that persists plugin-loader state to BoltDB.
//
// It embeds *craft.BoltFSM to reuse the snapshot/restore/close machinery
// (BoltDB file-level snapshot via tx.WriteTo / file replace on restore)
// while overriding Apply to dispatch loader-specific operations.
type LoaderFSM struct {
	*craft.BoltFSM
}

// NewLoaderFSM creates a LoaderFSM backed by a BoltDB file at path and
// initializes the four loader-specific top-level buckets if missing.
func NewLoaderFSM(path string) (*LoaderFSM, error) {
	boltFSM, err := craft.NewBoltFSM(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create bolt FSM for loader: %w", err)
	}
	fsm := &LoaderFSM{BoltFSM: boltFSM}

	if err := fsm.initBuckets(); err != nil {
		boltFSM.Close()
		return nil, fmt.Errorf("failed to initialize loader buckets: %w", err)
	}
	return fsm, nil
}

// NewLoaderFSMFromDir is a convenience constructor that places the FSM
// database file at <dataDir>/loader-fsm.db.
func NewLoaderFSMFromDir(dataDir string) (*LoaderFSM, error) {
	return NewLoaderFSM(filepath.Join(dataDir, "loader-fsm.db"))
}

func (f *LoaderFSM) initBuckets() error {
	db := f.BoltFSM.DB()
	if db == nil {
		return fmt.Errorf("loader FSM db is nil")
	}
	return db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{
			BucketPluginRegistry,
			BucketTrustedKeys,
			BucketPluginState,
			BucketHookRegistry,
		} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

// Apply dispatches a Raft log entry to the loader state machine.
//
// Returns *craft.FSMApplyResult (compatible with the existing BoltFSM
// convention) so the caller can inspect success/error in a uniform way.
func (f *LoaderFSM) Apply(log *hashicraft.Log) interface{} {
	var op LoaderFSMOp
	if err := json.Unmarshal(log.Data, &op); err != nil {
		return &craft.FSMApplyResult{
			Success: false,
			Error:   fmt.Sprintf("loader FSM: failed to unmarshal op: %v", err),
		}
	}

	if f.BoltFSM.DB() == nil {
		return &craft.FSMApplyResult{
			Success: false,
			Error:   "loader FSM: underlying bolt db is nil",
		}
	}

	var applyErr error
	switch op.Type {
	case OpPluginInstall:
		applyErr = f.applyPluginInstall(&op)
	case OpPluginUninstall:
		applyErr = f.applyPluginUninstall(&op)
	case OpTrustedKeyAdd:
		applyErr = f.applyTrustedKeyAdd(&op)
	case OpTrustedKeyRemove:
		applyErr = f.applyTrustedKeyRemove(&op)
	case OpStatePut:
		applyErr = f.applyStatePut(&op)
	case OpStateDelete:
		applyErr = f.applyStateDelete(&op)
	case OpHookRegister:
		applyErr = f.applyHookRegister(&op)
	case OpHookUnregister:
		applyErr = f.applyHookUnregister(&op)
	default:
		return &craft.FSMApplyResult{
			Success: false,
			Error:   fmt.Sprintf("loader FSM: unknown op type %q", op.Type),
		}
	}
	if applyErr != nil {
		return &craft.FSMApplyResult{Success: false, Error: applyErr.Error()}
	}
	return &craft.FSMApplyResult{Success: true}
}

// LoaderFSMOp is the unit of replication for the loader state machine.
//
// Field semantics depend on Type:
//
//   - OpPluginInstall       : Plugin=<name>, Data=PluginRecord (JSON)
//   - OpPluginUninstall     : Plugin=<name>
//   - OpTrustedKeyAdd       : Key=<key_id>, Data=TrustedKey (JSON)
//   - OpTrustedKeyRemove    : Key=<key_id>
//   - OpStatePut            : Plugin=<name>, Key=<state_key>, Data=raw value bytes,
//                            ExpectedVersion used for CAS (0 = no CAS)
//   - OpStateDelete         : Plugin=<name>, Key=<state_key>
//   - OpHookRegister        : Plugin=<name>, Key=<hook_id>, Data=HookRecord (JSON)
//   - OpHookUnregister      : Plugin=<name>, Key=<hook_id>
//
// Schema evolution rule (spec §12.4): only add fields with omitempty; never
// change a field's semantic meaning; never reuse a Type string for a
// different operation.
type LoaderFSMOp struct {
	Type            string `json:"type"`
	Plugin          string `json:"plugin,omitempty"`
	Key             string `json:"key,omitempty"`
	Data            []byte `json:"data,omitempty"`
	ExpectedVersion uint64 `json:"expected_version,omitempty"`
}

// Op type constants. Adding a new op type is a minor api_version bump
// (spec §3.15): older loaders will reject unknown ops with an explicit
// error, so a follower running an older loader will not silently drop
// the new op.
const (
	OpPluginInstall   = "plugin_install"
	OpPluginUninstall = "plugin_uninstall"
	OpTrustedKeyAdd   = "trusted_key_add"
	OpTrustedKeyRemove = "trusted_key_remove"
	OpStatePut        = "state_put"
	OpStateDelete     = "state_delete"
	OpHookRegister    = "hook_register"
	OpHookUnregister  = "hook_unregister"
)

// PluginRecord persists metadata about an installed plugin.
//
// Stored under BucketPluginRegistry with key = plugin name.
type PluginRecord struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	OCIURL      string `json:"oci_url"`
	SignatureB64 string `json:"signature_b64"`
	KeyID       string `json:"key_id"`
	TrustTier   int    `json:"trust_tier"`
	Manifest    json.RawMessage `json:"manifest"`
	InstalledAt string `json:"installed_at"`
	WASMBytes   []byte `json:"-"` // not persisted; loaded from OCI cache on demand
}

// TrustedKey persists an Ed25519 public key with trust tier flag.
//
// Stored under BucketTrustedKeys with key = key_id.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"` // ed25519 base64
	IsCore    bool   `json:"is_core"`
	AddedAt   string `json:"added_at"`
	AddedBy   string `json:"added_by"` // admin user ARN
}

// HookRecord persists a single S3 hook registration.
//
// Stored under BucketHookRegistry with key = "<plugin_name>:<hook_id>".
type HookRecord struct {
	PluginName string `json:"plugin_name"`
	HookID     string `json:"hook_id"`
	HookType   string `json:"hook_type"` // "before" | "after" | "around"
	Operation  string `json:"operation"`  // "s3:GetObject", ...
	Condition  string `json:"condition"`  // CEL expression
	Handler    string `json:"handler"`    // WASM entry function name
	Priority   int    `json:"priority"`
	OnFailure  string `json:"on_failure"` // "deny" | "allow" | "log_and_allow"
	Critical   bool   `json:"critical"`
}

// StateEntry is the per-key state value with version for CAS.
type StateEntry struct {
	Value   []byte `json:"value"`
	Version uint64 `json:"version"`
}

// --- Internal apply methods ---

func (f *LoaderFSM) applyPluginInstall(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketPluginRegistry))
		if b == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginRegistry)
		}
		return b.Put([]byte(op.Plugin), op.Data)
	})
}

func (f *LoaderFSM) applyPluginUninstall(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		// 1. Delete plugin from registry
		reg := tx.Bucket([]byte(BucketPluginRegistry))
		if reg == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginRegistry)
		}
		if err := reg.Delete([]byte(op.Plugin)); err != nil {
			return err
		}
		// 2. Delete plugin's nested state bucket
		stateRoot := tx.Bucket([]byte(BucketPluginState))
		if stateRoot == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginState)
		}
		if nested := stateRoot.Bucket([]byte(op.Plugin)); nested != nil {
			if err := stateRoot.DeleteBucket([]byte(op.Plugin)); err != nil {
				return err
			}
		}
		// 3. Delete hooks registered by this plugin
		hooks := tx.Bucket([]byte(BucketHookRegistry))
		if hooks == nil {
			return fmt.Errorf("bucket %s missing", BucketHookRegistry)
		}
		prefix := []byte(op.Plugin + ":")
		c := hooks.Cursor()
		var keysToDelete [][]byte
		for k, _ := c.Seek(prefix); k != nil && len(k) >= len(prefix) && string(k[:len(prefix)]) == string(prefix); k, _ = c.Next() {
			keysToDelete = append(keysToDelete, k)
		}
		for _, k := range keysToDelete {
			if err := hooks.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (f *LoaderFSM) applyTrustedKeyAdd(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTrustedKeys))
		if b == nil {
			return fmt.Errorf("bucket %s missing", BucketTrustedKeys)
		}
		return b.Put([]byte(op.Key), op.Data)
	})
}

func (f *LoaderFSM) applyTrustedKeyRemove(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketTrustedKeys))
		if b == nil {
			return fmt.Errorf("bucket %s missing", BucketTrustedKeys)
		}
		return b.Delete([]byte(op.Key))
	})
}

func (f *LoaderFSM) applyStatePut(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		stateRoot := tx.Bucket([]byte(BucketPluginState))
		if stateRoot == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginState)
		}
		nested, err := stateRoot.CreateBucketIfNotExists([]byte(op.Plugin))
		if err != nil {
			return err
		}

		// CAS check
		if op.ExpectedVersion > 0 {
			existing := nested.Get([]byte(op.Key))
			if existing != nil {
				var entry StateEntry
				if err := json.Unmarshal(existing, &entry); err != nil {
					return fmt.Errorf("corrupt state entry for %s/%s: %w", op.Plugin, op.Key, err)
				}
				if entry.Version != op.ExpectedVersion {
					return ErrVersionMismatch{
						Plugin:          op.Plugin,
						Key:             op.Key,
						Expected:        op.ExpectedVersion,
						Actual:          entry.Version,
					}
				}
			} else {
				// Key does not exist; expected_version must be 0 for create.
				// (Allowing nonzero expected_version against a non-existent key
				// would be a CAS correctness hole.)
				return ErrVersionMismatch{
					Plugin:   op.Plugin,
					Key:      op.Key,
					Expected: op.ExpectedVersion,
					Actual:   0,
				}
			}
		}

		// Compute new version
		var newVersion uint64 = 1
		if existing := nested.Get([]byte(op.Key)); existing != nil {
			var entry StateEntry
			if err := json.Unmarshal(existing, &entry); err == nil {
				newVersion = entry.Version + 1
			}
		}
		entry := StateEntry{
			Value:   op.Data,
			Version: newVersion,
		}
		data, err := json.Marshal(&entry)
		if err != nil {
			return fmt.Errorf("failed to marshal state entry: %w", err)
		}
		return nested.Put([]byte(op.Key), data)
	})
}

func (f *LoaderFSM) applyStateDelete(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		stateRoot := tx.Bucket([]byte(BucketPluginState))
		if stateRoot == nil {
			return fmt.Errorf("bucket %s missing", BucketPluginState)
		}
		nested := stateRoot.Bucket([]byte(op.Plugin))
		if nested == nil {
			return nil // idempotent
		}
		return nested.Delete([]byte(op.Key))
	})
}

func (f *LoaderFSM) applyHookRegister(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketHookRegistry))
		if b == nil {
			return fmt.Errorf("bucket %s missing", BucketHookRegistry)
		}
		key := op.Plugin + ":" + op.Key
		return b.Put([]byte(key), op.Data)
	})
}

func (f *LoaderFSM) applyHookUnregister(op *LoaderFSMOp) error {
	return f.DB().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(BucketHookRegistry))
		if b == nil {
			return fmt.Errorf("bucket %s missing", BucketHookRegistry)
		}
		key := op.Plugin + ":" + op.Key
		return b.Delete([]byte(key))
	})
}

// ErrVersionMismatch is returned by applyStatePut when a CAS check fails.
// Callers should return this to the WASM guest as the ErrVersionConflict
// host import error (spec §3.14).
type ErrVersionMismatch struct {
	Plugin   string
	Key      string
	Expected uint64
	Actual   uint64
}

func (e ErrVersionMismatch) Error() string {
	return fmt.Sprintf("state CAS mismatch for plugin=%s key=%s: expected version %d, got %d",
		e.Plugin, e.Key, e.Expected, e.Actual)
}

// IsErrVersionMismatch reports whether err is a CAS mismatch.
func IsErrVersionMismatch(err error) bool {
	_, ok := err.(ErrVersionMismatch)
	return ok
}

// compile-time assertion: LoaderFSM satisfies hashicorp/raft.FSM
var _ hashicraft.FSM = (*LoaderFSM)(nil)
