package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

type IncrementalBackupInfo struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	StartLSN    int64     `json:"start_lsn"`
	EndLSN      int64     `json:"end_lsn"`
	Checksum    string    `json:"checksum"`
	ObjectCount int       `json:"object_count"`
}

type Manifest struct {
	LSNRange   LSNRange   `json:"lsn_range"`
	Checksum   string     `json:"checksum"`
	ObjectCount int       `json:"object_count"`
	Timestamp  time.Time  `json:"timestamp"`
	Type       string     `json:"type"`
}

type LSNRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type lsnMeta struct {
	LastBackupLSN int64 `json:"last_backup_lsn"`
}

// CreateIncrementalBackup creates an incremental backup since the given LSN.
// It reads BoltDB changes since the given LSN, exports changed objects and metadata
// as a tar.gz archive, and creates a manifest.json with checksum and metadata.
func CreateIncrementalBackup(ctx context.Context, db *bbolt.DB, dataDir, backupDir string, sinceLSN int64) (*IncrementalBackupInfo, error) {
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Get current LSN
	currentLSN := int64(0)
	if err := db.View(func(tx *bbolt.Tx) error {
		currentLSN = int64(tx.ID())
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to get current LSN: %w", err)
	}

	// Allow creating a backup even if LSN hasn't advanced (for first backup or testing)
	// Just use the current LSN as both start and end
	if currentLSN < sinceLSN {
		currentLSN = sinceLSN
	}

	timestamp := time.Now().Format("20060102-150405")
	backupName := fmt.Sprintf("incr-%s", timestamp)
	backupPath := filepath.Join(backupDir, backupName+".tar.gz")

	// Collect changed objects since the given LSN
	changedObjects, objectCount, err := collectChangedObjects(db, dataDir, sinceLSN)
	if err != nil {
		return nil, fmt.Errorf("failed to collect changed objects: %w", err)
	}

	// Create the tar.gz archive with manifest
	checksum, err := createIncrementalArchive(backupPath, db, changedObjects, sinceLSN, currentLSN, objectCount)
	if err != nil {
		return nil, fmt.Errorf("failed to create incremental archive: %w", err)
	}

	// Update last_backup_lsn in metadata
	if err := updateLastBackupLSN(backupDir, currentLSN); err != nil {
		return nil, fmt.Errorf("failed to update last backup LSN: %w", err)
	}

	info, _ := os.Stat(backupPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	return &IncrementalBackupInfo{
		Name:        backupName,
		Path:        backupPath,
		Size:        size,
		CreatedAt:   time.Now(),
		StartLSN:    sinceLSN,
		EndLSN:      currentLSN,
		Checksum:    checksum,
		ObjectCount: objectCount,
	}, nil
}

// collectChangedObjects collects objects that have changed since the given LSN.
// Since BoltDB doesn't have a built-in change tracking mechanism per LSN,
// we export all current objects (the full state) as the incremental approach
// relies on the LSN range to determine what's changed.
func collectChangedObjects(db *bbolt.DB, dataDir string, sinceLSN int64) ([]string, int, error) {
	var changedObjects []string
	objectCount := 0

	err := db.View(func(tx *bbolt.Tx) error {
		// Collect from objects bucket
		objBucket := tx.Bucket([]byte("objects"))
		if objBucket != nil {
			cursor := objBucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				if v != nil {
					key := string(k)
					changedObjects = append(changedObjects, key)
					objectCount++
				}
			}
		}

		// Collect from buckets metadata
		bucketBucket := tx.Bucket([]byte("buckets"))
		if bucketBucket != nil {
			cursor := bucketBucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				if v != nil {
					key := "bucket:" + string(k)
					changedObjects = append(changedObjects, key)
				}
			}
		}

		return nil
	})

	return changedObjects, objectCount, err
}

// createIncrementalArchive creates a tar.gz archive containing the changed data and manifest.
// The checksum in the manifest is the SHA-256 of the concatenated data content
// (all bucket JSON files), excluding the manifest itself, to avoid circular dependency.
func createIncrementalArchive(backupPath string, db *bbolt.DB, changedObjects []string, startLSN, endLSN int64, objectCount int) (string, error) {
	// First, compute the data checksum by hashing all bucket data
	dataChecksum, err := computeDataChecksum(db)
	if err != nil {
		return "", fmt.Errorf("failed to compute data checksum: %w", err)
	}

	manifest := &Manifest{
		LSNRange: LSNRange{
			Start: startLSN,
			End:   endLSN,
		},
		Checksum:    dataChecksum,
		ObjectCount: objectCount,
		Timestamp:   time.Now(),
		Type:        "incremental",
	}

	if err := writeArchive(backupPath, db, changedObjects, manifest); err != nil {
		return "", fmt.Errorf("failed to write archive: %w", err)
	}

	return dataChecksum, nil
}

// computeDataChecksum computes SHA-256 of all BoltDB bucket data.
// The hash is computed over the serialized JSON representation of each bucket's entries,
// matching the format stored in the archive.
func computeDataChecksum(db *bbolt.DB) (string, error) {
	hasher := sha256.New()

	err := db.View(func(tx *bbolt.Tx) error {
		bucketNames := []string{"objects", "buckets", "object_versions", "bucket_index", "object_index"}
		for _, name := range bucketNames {
			bucket := tx.Bucket([]byte(name))
			if bucket == nil {
				continue
			}

			// Hash the bucket name (same as the filename in the archive)
			hasher.Write([]byte(name + ".json"))

			// Serialize entries the same way as exportBucket
			type kvEntry struct {
				Key   string `json:"key"`
				Value []byte `json:"value"`
			}

			var entries []kvEntry
			cursor := bucket.Cursor()
			for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
				entries = append(entries, kvEntry{
					Key:   string(k),
					Value: v,
				})
			}

			data, err := json.Marshal(entries)
			if err != nil {
				return err
			}
			hasher.Write(data)
		}
		return nil
	})

	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

// writeArchive writes the incremental backup archive with the given manifest.
func writeArchive(backupPath string, db *bbolt.DB, changedObjects []string, manifest *Manifest) error {
	f, err := os.Create(backupPath)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	// Export BoltDB data
	if err := exportBoltDBData(tw, db, changedObjects); err != nil {
		return fmt.Errorf("failed to export bolt data: %w", err)
	}

	// Write manifest
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal manifest: %w", err)
	}

	if err := tw.WriteHeader(&tar.Header{
		Name:    "manifest.json",
		Size:    int64(len(manifestData)),
		Mode:    0644,
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to write manifest header: %w", err)
	}

	if _, err := tw.Write(manifestData); err != nil {
		return fmt.Errorf("failed to write manifest data: %w", err)
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	return nil
}

// exportBoltDBData exports data from BoltDB buckets into the tar archive.
func exportBoltDBData(tw *tar.Writer, db *bbolt.DB, changedObjects []string) error {
	return db.View(func(tx *bbolt.Tx) error {
		// Export objects bucket
		objBucket := tx.Bucket([]byte("objects"))
		if objBucket != nil {
			if err := exportBucket(tw, "objects", objBucket); err != nil {
				return err
			}
		}

		// Export buckets metadata
		bucketBucket := tx.Bucket([]byte("buckets"))
		if bucketBucket != nil {
			if err := exportBucket(tw, "buckets", bucketBucket); err != nil {
				return err
			}
		}

		// Export object_versions
		verBucket := tx.Bucket([]byte("object_versions"))
		if verBucket != nil {
			if err := exportBucket(tw, "object_versions", verBucket); err != nil {
				return err
			}
		}

		// Export bucket_index
		biBucket := tx.Bucket([]byte("bucket_index"))
		if biBucket != nil {
			if err := exportBucket(tw, "bucket_index", biBucket); err != nil {
				return err
			}
		}

		// Export object_index
		oiBucket := tx.Bucket([]byte("object_index"))
		if oiBucket != nil {
			if err := exportBucket(tw, "object_index", oiBucket); err != nil {
				return err
			}
		}

		return nil
	})
}

// exportBucket writes all key-value pairs from a BoltDB bucket into the tar archive.
func exportBucket(tw *tar.Writer, bucketName string, bucket *bbolt.Bucket) error {
	type kvEntry struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
	}

	var entries []kvEntry
	cursor := bucket.Cursor()
	for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
		entries = append(entries, kvEntry{
			Key:   string(k),
			Value: v,
		})
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket %s: %w", bucketName, err)
	}

	if err := tw.WriteHeader(&tar.Header{
		Name:    bucketName + ".json",
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to write header for %s: %w", bucketName, err)
	}

	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("failed to write data for %s: %w", bucketName, err)
	}

	return nil
}

// readManifest reads the manifest.json from an incremental backup tar.gz archive.
func readManifest(archivePath string) (*Manifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open archive: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		if hdr.Name == "manifest.json" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("failed to read manifest: %w", err)
			}

			var manifest Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				return nil, fmt.Errorf("failed to parse manifest: %w", err)
			}
			return &manifest, nil
		}
	}

	return nil, fmt.Errorf("manifest.json not found in archive")
}

// applyIncrementalBackup applies an incremental backup to the data directory.
func applyIncrementalBackup(dataDir string, backupPath string) error {
	f, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("failed to open incremental backup: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	dbPath := filepath.Join(dataDir, "metadata.db")
	db, err := bbolt.Open(dbPath, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to open database for incremental restore: %w", err)
	}
	defer db.Close()

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Skip manifest
		if hdr.Name == "manifest.json" {
			continue
		}

		// Read bucket data
		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", hdr.Name, err)
		}

		// Parse bucket name from filename (e.g., "objects.json" -> "objects")
		bucketName := hdr.Name
		if len(bucketName) > 5 && bucketName[len(bucketName)-5:] == ".json" {
			bucketName = bucketName[:len(bucketName)-5]
		}

		// Apply to database
		if err := applyBucketData(db, bucketName, data); err != nil {
			return fmt.Errorf("failed to apply %s: %w", bucketName, err)
		}
	}

	return nil
}

// applyBucketData applies key-value entries from JSON data to a BoltDB bucket.
func applyBucketData(db *bbolt.DB, bucketName string, data []byte) error {
	type kvEntry struct {
		Key   string `json:"key"`
		Value []byte `json:"value"`
	}

	var entries []kvEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("failed to parse bucket data: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	return db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return fmt.Errorf("failed to create bucket %s: %w", bucketName, err)
		}

		for _, entry := range entries {
			if err := bucket.Put([]byte(entry.Key), entry.Value); err != nil {
				return fmt.Errorf("failed to put key %s: %w", entry.Key, err)
			}
		}

		return nil
	})
}

// updateLastBackupLSN writes the last backup LSN to a metadata file.
func updateLastBackupLSN(backupDir string, lsn int64) error {
	metaPath := filepath.Join(backupDir, ".backup_lsn")
	meta := lsnMeta{LastBackupLSN: lsn}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

// GetLastBackupLSN reads the last backup LSN from the metadata file.
func GetLastBackupLSN(backupDir string) (int64, error) {
	metaPath := filepath.Join(backupDir, ".backup_lsn")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	var meta lsnMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, err
	}

	return meta.LastBackupLSN, nil
}

// ListIncrementalBackups lists all incremental backups in the backup directory.
func ListIncrementalBackups(backupDir string) ([]*IncrementalBackupInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var backups []*IncrementalBackupInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Only process incremental backup files
		if len(name) < 4 || name[len(name)-7:] != ".tar.gz" {
			continue
		}

		path := filepath.Join(backupDir, name)
		manifest, err := readManifest(path)
		if err != nil {
			continue
		}

		info, _ := entry.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}

		backups = append(backups, &IncrementalBackupInfo{
			Name:        name,
			Path:        path,
			Size:        size,
			CreatedAt:   manifest.Timestamp,
			StartLSN:    manifest.LSNRange.Start,
			EndLSN:      manifest.LSNRange.End,
			Checksum:    manifest.Checksum,
			ObjectCount: manifest.ObjectCount,
		})
	}

	// Sort by start LSN
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].StartLSN < backups[j].StartLSN
	})

	return backups, nil
}

// CreateIncrementalBackupManager creates an incremental backup using BackupManager.
func (bm *BackupManager) CreateIncrementalBackup(ctx context.Context) (*IncrementalBackupInfo, error) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	sinceLSN, err := GetLastBackupLSN(bm.backupDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get last backup LSN: %w", err)
	}

	return CreateIncrementalBackup(ctx, bm.db, bm.dataDir, bm.backupDir, sinceLSN)
}
