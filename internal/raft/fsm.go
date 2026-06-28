package raft

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
)

// BoltFSM implements the raft.FSM interface using BoltDB as the state machine.
type BoltFSM struct {
	mu   sync.RWMutex
	db   *bolt.DB
	path string
}

// NewBoltFSM creates a new BoltFSM with the given database path.
func NewBoltFSM(path string) (*BoltFSM, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create FSM directory: %w", err)
	}

	db, err := bolt.Open(path, 0666, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open FSM bolt db: %w", err)
	}

	// Initialize buckets
	if err := db.Update(func(tx *bolt.Tx) error {
		buckets := []string{
			"buckets",
			"objects",
			"object_versions",
			"uploads",
			"upload_parts",
			"access_history",
			"vectors",
			"pipelines",
			"bucket_index",
			"object_index",
		}
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize FSM buckets: %w", err)
	}

	return &BoltFSM{
		db:   db,
		path: path,
	}, nil
}

// Apply applies a Raft log entry to the BoltDB state machine.
func (f *BoltFSM) Apply(log *raft.Log) interface{} {
	var op FSMOperation
	if err := json.Unmarshal(log.Data, &op); err != nil {
		return &FSMApplyResult{
			Success: false,
			Error:   fmt.Sprintf("failed to unmarshal operation: %v", err),
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var applyErr error
	switch op.Type {
	case "put_object":
		applyErr = f.applyPutObject(&op)
	case "delete_object":
		applyErr = f.applyDeleteObject(&op)
	case "create_bucket":
		applyErr = f.applyCreateBucket(&op)
	case "delete_bucket":
		applyErr = f.applyDeleteBucket(&op)
	case "update_bucket":
		applyErr = f.applyUpdateBucket(&op)
	case "put_object_version":
		applyErr = f.applyPutObjectVersion(&op)
	case "put_upload":
		applyErr = f.applyPutUpload(&op)
	case "delete_upload":
		applyErr = f.applyDeleteUpload(&op)
	case "add_part":
		applyErr = f.applyAddPart(&op)
	default:
		return &FSMApplyResult{
			Success: false,
			Error:   fmt.Sprintf("unknown operation type: %s", op.Type),
		}
	}

	if applyErr != nil {
		return &FSMApplyResult{
			Success: false,
			Error:   applyErr.Error(),
		}
	}

	return &FSMApplyResult{Success: true}
}

// Snapshot creates a BoltDB snapshot for the FSM.
func (f *BoltFSM) Snapshot() (raft.FSMSnapshot, error) {
	return &BoltSnapshot{fsm: f}, nil
}

// Restore restores the FSM from a snapshot.
func (f *BoltFSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Close the current database
	f.db.Close()

	// Restore from the snapshot by writing to a temp file then replacing
	tmpPath := f.path + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file for restore: %w", err)
	}

	if _, err := tmpFile.ReadFrom(rc); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to read snapshot data: %w", err)
	}
	tmpFile.Close()

	// Replace the old database file with the restored one
	if err := os.Rename(tmpPath, f.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to replace database file: %w", err)
	}

	// Reopen the database
	db, err := bolt.Open(f.path, 0666, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to reopen database after restore: %w", err)
	}
	f.db = db

	return nil
}

// Close closes the underlying BoltDB database.
func (f *BoltFSM) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.db != nil {
		return f.db.Close()
	}
	return nil
}

// DB returns the underlying BoltDB database for read operations.
func (f *BoltFSM) DB() *bolt.DB {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.db
}

// --- Internal apply methods ---

func (f *BoltFSM) applyPutObject(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		key := []byte(op.Bucket + "/" + op.Key)
		if err := objBucket.Put(key, op.Data); err != nil {
			return err
		}

		indexBucket := tx.Bucket([]byte("object_index"))
		prefix := extractPrefix(op.Key)
		indexKey := []byte(op.Bucket + "/" + prefix)
		existing := indexBucket.Get(indexKey)
		if existing == nil {
			indexBucket.Put(indexKey, op.Data)
		}

		return nil
	})
}

func (f *BoltFSM) applyDeleteObject(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		key := []byte(op.Bucket + "/" + op.Key)
		return objBucket.Delete(key)
	})
}

func (f *BoltFSM) applyCreateBucket(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		return bucketBucket.Put([]byte(op.Bucket), op.Data)
	})
}

func (f *BoltFSM) applyDeleteBucket(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		// Delete all objects in the bucket
		objBucket := tx.Bucket([]byte("objects"))
		prefix := []byte(op.Bucket + "/")
		cursor := objBucket.Cursor()
		var keysToDelete [][]byte
		for k, _ := cursor.Seek(prefix); k != nil; k, _ = cursor.Next() {
			if len(k) < len(prefix) || string(k[:len(prefix)]) != string(prefix) {
				break
			}
			keysToDelete = append(keysToDelete, k)
		}
		for _, k := range keysToDelete {
			objBucket.Delete(k)
		}

		bucketBucket := tx.Bucket([]byte("buckets"))
		return bucketBucket.Delete([]byte(op.Bucket))
	})
}

func (f *BoltFSM) applyUpdateBucket(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		return bucketBucket.Put([]byte(op.Bucket), op.Data)
	})
}

func (f *BoltFSM) applyPutObjectVersion(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		verBucket := tx.Bucket([]byte("object_versions"))
		// Data should contain version info; key format: bucket/key/versionID
		key := []byte(op.Bucket + "/" + op.Key)
		return verBucket.Put(key, op.Data)
	})
}

func (f *BoltFSM) applyPutUpload(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		key := []byte(op.Bucket + "/" + op.Key)
		return uploadBucket.Put(key, op.Data)
	})
}

func (f *BoltFSM) applyDeleteUpload(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		uploadBucket := tx.Bucket([]byte("uploads"))
		key := []byte(op.Bucket + "/" + op.Key)
		return uploadBucket.Delete(key)
	})
}

func (f *BoltFSM) applyAddPart(op *FSMOperation) error {
	return f.db.Update(func(tx *bolt.Tx) error {
		partsBucket := tx.Bucket([]byte("upload_parts"))
		key := []byte(op.Bucket + "/" + op.Key)
		return partsBucket.Put(key, op.Data)
	})
}

// extractPrefix extracts the prefix (directory part) from a key.
func extractPrefix(key string) string {
	lastSlash := -1
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			lastSlash = i
			break
		}
	}
	if lastSlash == -1 {
		return ""
	}
	return key[:lastSlash+1]
}
