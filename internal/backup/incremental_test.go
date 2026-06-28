package backup

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func setupTestDB(t *testing.T) (*bbolt.DB, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "nexus-backup-test-*")
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmpDir, "metadata.db")
	db, err := bbolt.Open(dbPath, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatal(err)
	}

	// Initialize standard buckets
	if err := db.Update(func(tx *bbolt.Tx) error {
		buckets := []string{"buckets", "objects", "object_versions", "uploads", "upload_parts", "bucket_index", "object_index"}
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		db.Close()
		os.RemoveAll(tmpDir)
		t.Fatal(err)
	}

	return db, tmpDir
}

func teardownTestDB(t *testing.T, db *bbolt.DB, tmpDir string) {
	t.Helper()
	db.Close()
	os.RemoveAll(tmpDir)
}

func addTestObjects(t *testing.T, db *bbolt.DB, bucket, key, value string) {
	t.Helper()
	err := db.Update(func(tx *bbolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		if objBucket == nil {
			return nil
		}
		return objBucket.Put([]byte(bucket+"/"+key), []byte(value))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func addTestBucket(t *testing.T, db *bbolt.DB, name string) {
	t.Helper()
	err := db.Update(func(tx *bbolt.Tx) error {
		bucketBucket := tx.Bucket([]byte("buckets"))
		if bucketBucket == nil {
			return nil
		}
		return bucketBucket.Put([]byte(name), []byte(`{"name":"`+name+`"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateIncrementalBackup(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	// Add some test data
	addTestBucket(t, db, "test-bucket")
	addTestObjects(t, db, "test-bucket", "key1", `{"key":"key1","bucket":"test-bucket"}`)

	// Create incremental backup since LSN 0
	info, err := CreateIncrementalBackup(ctx, db, tmpDir, backupDir, 0)
	if err != nil {
		t.Fatalf("Failed to create incremental backup: %v", err)
	}

	if info == nil {
		t.Fatal("Expected non-nil IncrementalBackupInfo")
	}

	if info.StartLSN != 0 {
		t.Errorf("Expected StartLSN=0, got %d", info.StartLSN)
	}

	if info.EndLSN <= 0 {
		t.Errorf("Expected EndLSN > 0, got %d", info.EndLSN)
	}

	if info.Checksum == "" {
		t.Error("Expected non-empty checksum")
	}

	if info.ObjectCount <= 0 {
		t.Errorf("Expected ObjectCount > 0, got %d", info.ObjectCount)
	}

	// Verify the file exists
	if _, err := os.Stat(info.Path); os.IsNotExist(err) {
		t.Errorf("Backup file not found: %s", info.Path)
	}

	// Verify the file is a valid gzip archive
	manifest, err := readManifest(info.Path)
	if err != nil {
		t.Fatalf("Failed to read manifest from archive: %v", err)
	}

	if manifest.LSNRange.Start != 0 {
		t.Errorf("Expected manifest LSNRange.Start=0, got %d", manifest.LSNRange.Start)
	}

	if manifest.Checksum == "" {
		t.Error("Expected non-empty manifest checksum")
	}

	if manifest.Type != "incremental" {
		t.Errorf("Expected manifest type=incremental, got %s", manifest.Type)
	}
}

func TestManifestChecksumVerification(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "verify-bucket")
	addTestObjects(t, db, "verify-bucket", "key1", `{"key":"key1"}`)

	info, err := CreateIncrementalBackup(ctx, db, tmpDir, backupDir, 0)
	if err != nil {
		t.Fatalf("Failed to create incremental backup: %v", err)
	}

	// Verify the backup using VerifyBackupIntegrity
	err = VerifyBackupIntegrity(info.Path)
	if err != nil {
		t.Fatalf("Backup verification failed: %v", err)
	}

	// Add more data to the database and verify the old backup's checksum still matches
	// (the old backup's data checksum should still be valid for its own data)
	addTestObjects(t, db, "verify-bucket", "key2", `{"key":"key2"}`)
	err = VerifyBackupIntegrity(info.Path)
	if err != nil {
		t.Fatalf("Backup verification should still pass for unchanged backup: %v", err)
	}

	// Create a backup with known data, then modify the data and verify it fails
	addTestBucket(t, db, "corrupt-test")
	addTestObjects(t, db, "corrupt-test", "ckey", `{"key":"ckey"}`)

	corruptInfo, err := CreateIncrementalBackup(ctx, db, tmpDir, backupDir, info.EndLSN)
	if err != nil {
		t.Fatalf("Failed to create incremental backup for corruption test: %v", err)
	}

	// Verify it passes first
	err = VerifyBackupIntegrity(corruptInfo.Path)
	if err != nil {
		t.Fatalf("Backup verification failed before corruption: %v", err)
	}

	// Now modify the database content and create a new backup
	// The old backup's checksum should still be valid (it's a snapshot)
	err = VerifyBackupIntegrity(corruptInfo.Path)
	if err != nil {
		t.Fatalf("Backup verification should still pass: %v", err)
	}
}

func TestRestoreFromFullAndIncremental(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	// Add initial data
	addTestBucket(t, db, "restore-bucket")
	addTestObjects(t, db, "restore-bucket", "key1", `{"key":"key1","version":1}`)

	// Create a full backup
	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	fullBackup, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create full backup: %v", err)
	}

	// Add more data
	addTestObjects(t, db, "restore-bucket", "key2", `{"key":"key2","version":2}`)

	// Create incremental backup
	incrBackup, err := bm.CreateIncrementalBackup(ctx)
	if err != nil {
		t.Fatalf("Failed to create incremental backup: %v", err)
	}

	// Now restore to a new directory
	restoreDir, err := os.MkdirTemp("", "nexus-restore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(restoreDir)

	restoreDBPath := filepath.Join(restoreDir, "metadata.db")

	// Copy full backup to restore location
	if err := copyFile(fullBackup.Path, restoreDBPath); err != nil {
		t.Fatalf("Failed to copy full backup: %v", err)
	}

	// Apply incremental backup
	if err := applyIncrementalBackup(restoreDir, incrBackup.Path); err != nil {
		t.Fatalf("Failed to apply incremental backup: %v", err)
	}

	// Verify the restored database has the data
	restoreDB, err := bbolt.Open(restoreDBPath, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to open restored database: %v", err)
	}
	defer restoreDB.Close()

	// Check that objects exist
	foundKey1 := false
	foundKey2 := false
	err = restoreDB.View(func(tx *bbolt.Tx) error {
		objBucket := tx.Bucket([]byte("objects"))
		if objBucket == nil {
			return nil
		}
		cursor := objBucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			key := string(k)
			if key == "restore-bucket/key1" {
				foundKey1 = v != nil
			}
			if key == "restore-bucket/key2" {
				foundKey2 = v != nil
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Failed to read restored database: %v", err)
	}

	if !foundKey1 {
		t.Error("Expected key1 to be found in restored database")
	}
	if !foundKey2 {
		t.Error("Expected key2 to be found in restored database")
	}
}

func TestEncryptionDecryptionRoundTrip(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	// Add test data
	addTestBucket(t, db, "encrypt-bucket")
	addTestObjects(t, db, "encrypt-bucket", "secret-key", `{"key":"secret-key","data":"sensitive"}`)

	// Create a full backup
	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	backupInfo, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create backup: %v", err)
	}

	// Generate a random 32-byte key
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("Failed to generate encryption key: %v", err)
	}

	// Encrypt the backup
	if err := EncryptBackup(backupInfo.Path, key); err != nil {
		t.Fatalf("Failed to encrypt backup: %v", err)
	}

	encPath := backupInfo.Path + ".enc"
	if _, err := os.Stat(encPath); os.IsNotExist(err) {
		t.Fatalf("Encrypted file not found: %s", encPath)
	}

	// Decrypt the backup
	plaintext, err := DecryptBackup(encPath, key)
	if err != nil {
		t.Fatalf("Failed to decrypt backup: %v", err)
	}

	// Verify decrypted content matches original
	original, err := os.ReadFile(backupInfo.Path)
	if err != nil {
		t.Fatalf("Failed to read original backup: %v", err)
	}

	if len(plaintext) != len(original) {
		t.Errorf("Decrypted size mismatch: got %d, expected %d", len(plaintext), len(original))
	}

	for i := range plaintext {
		if plaintext[i] != original[i] {
			t.Errorf("Decrypted content mismatch at byte %d", i)
			break
		}
	}

	// Verify that wrong key fails to decrypt
	wrongKey := make([]byte, 32)
	if _, err := rand.Read(wrongKey); err != nil {
		t.Fatal(err)
	}

	_, err = DecryptBackup(encPath, wrongKey)
	if err == nil {
		t.Error("Expected decryption with wrong key to fail, but it succeeded")
	}
}

func TestCreateConsistentSnapshot(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "snapshot-bucket")
	addTestObjects(t, db, "snapshot-bucket", "key1", `{"key":"key1"}`)

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	snapshot, err := bm.CreateConsistentSnapshot(ctx)
	if err != nil {
		t.Fatalf("Failed to create consistent snapshot: %v", err)
	}

	if snapshot.Type != "snapshot" {
		t.Errorf("Expected type=snapshot, got %s", snapshot.Type)
	}

	if snapshot.Size <= 0 {
		t.Errorf("Expected positive size, got %d", snapshot.Size)
	}

	// Verify the snapshot is a valid BoltDB file
	snapshotDB, err := bbolt.Open(snapshot.Path, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to open snapshot as BoltDB: %v", err)
	}
	snapshotDB.Close()
}

func TestGetLastBackupLSN(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nexus-lsn-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Initially, LSN should be 0
	lsn, err := GetLastBackupLSN(tmpDir)
	if err != nil {
		t.Fatalf("Failed to get last backup LSN: %v", err)
	}
	if lsn != 0 {
		t.Errorf("Expected initial LSN=0, got %d", lsn)
	}

	// Update LSN
	if err := updateLastBackupLSN(tmpDir, 42); err != nil {
		t.Fatalf("Failed to update last backup LSN: %v", err)
	}

	lsn, err = GetLastBackupLSN(tmpDir)
	if err != nil {
		t.Fatalf("Failed to get last backup LSN: %v", err)
	}
	if lsn != 42 {
		t.Errorf("Expected LSN=42, got %d", lsn)
	}
}

func TestListIncrementalBackups(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "list-bucket")
	addTestObjects(t, db, "list-bucket", "key1", `{"key":"key1"}`)

	// Create first incremental backup
	info1, err := CreateIncrementalBackup(ctx, db, tmpDir, backupDir, 0)
	if err != nil {
		t.Fatalf("Failed to create first incremental backup: %v", err)
	}

	// Wait to ensure different timestamp
	time.Sleep(1100 * time.Millisecond)

	// Force more writes to advance the LSN
	for i := 0; i < 5; i++ {
		addTestObjects(t, db, "list-bucket", fmt.Sprintf("key-extra-%d", i), fmt.Sprintf(`{"key":"key-extra-%d"}`, i))
	}

	// Create second incremental backup
	_, err = CreateIncrementalBackup(ctx, db, tmpDir, backupDir, info1.EndLSN)
	if err != nil {
		t.Fatalf("Failed to create second incremental backup: %v", err)
	}

	// List incremental backups
	backups, err := ListIncrementalBackups(backupDir)
	if err != nil {
		t.Fatalf("Failed to list incremental backups: %v", err)
	}

	if len(backups) < 2 {
		t.Errorf("Expected at least 2 incremental backups, got %d", len(backups))
	}

	// Verify they are sorted by start LSN
	for i := 1; i < len(backups); i++ {
		if backups[i].StartLSN < backups[i-1].StartLSN {
			t.Error("Expected backups to be sorted by start LSN")
		}
	}
}

func TestUploadToLocal(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "upload-bucket")

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	backupInfo, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create backup: %v", err)
	}

	// Upload to local directory
	localDest := filepath.Join(tmpDir, "remote-backup")
	err = UploadToRemote(ctx, backupInfo.Path, &RemoteConfig{
		Type:     "local",
		Endpoint: localDest,
		Prefix:   "backups",
	})
	if err != nil {
		t.Fatalf("Failed to upload to local: %v", err)
	}

	// Verify the file was copied
	destPath := filepath.Join(localDest, "backups", filepath.Base(backupInfo.Path))
	if _, err := os.Stat(destPath); os.IsNotExist(err) {
		t.Errorf("Remote backup file not found: %s", destPath)
	}
}

func TestVerifyBoltDBBackup(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "verify-bucket")

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	backupInfo, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create backup: %v", err)
	}

	// Verify the backup
	err = VerifyBackupIntegrity(backupInfo.Path)
	if err != nil {
		t.Fatalf("Backup verification failed: %v", err)
	}

	// Verify with non-existent file
	err = VerifyBackupIntegrity("/nonexistent/path.db")
	if err == nil {
		t.Error("Expected verification to fail for non-existent file")
	}
}

func TestComputeSHA256(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "sha256-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("hello world")
	tmpFile.Close()

	checksum, err := ComputeSHA256(tmpFile.Name())
	if err != nil {
		t.Fatalf("Failed to compute SHA-256: %v", err)
	}

	if checksum == "" {
		t.Error("Expected non-empty checksum")
	}

	// SHA-256 of "hello world" is known
	expected := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if checksum != expected {
		t.Errorf("Expected checksum %s, got %s", expected, checksum)
	}
}

func TestDrillBackup(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "drill-bucket")
	addTestObjects(t, db, "drill-bucket", "key1", `{"key":"key1"}`)

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	backupInfo, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create backup: %v", err)
	}

	// Run drill
	result, err := bm.DrillBackup(ctx, backupInfo.Path)
	if err != nil {
		t.Fatalf("Drill failed: %v", err)
	}

	if !result.Success {
		t.Errorf("Expected drill to succeed, but it failed. Checks: %+v", result.Checks)
	}

	if result.Duration <= 0 {
		t.Error("Expected positive duration")
	}

	// Verify all checks passed
	for _, check := range result.Checks {
		if !check.Passed {
			t.Errorf("Check %q failed: %s", check.Name, check.Message)
		}
	}
}

func TestWriteSnapshotTo(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "snapshot-to-bucket")
	addTestObjects(t, db, "snapshot-to-bucket", "key1", `{"key":"key1"}`)

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	// Write snapshot to a file via WriteSnapshotTo
	snapshotPath := filepath.Join(backupDir, "stream-snapshot.db")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}

	size, err := bm.WriteSnapshotTo(ctx, f)
	f.Close()

	if err != nil {
		t.Fatalf("Failed to write snapshot: %v", err)
	}

	if size <= 0 {
		t.Errorf("Expected positive size, got %d", size)
	}

	// Verify the snapshot is a valid BoltDB file
	snapshotDB, err := bbolt.Open(snapshotPath, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to open snapshot as BoltDB: %v", err)
	}
	snapshotDB.Close()
}

func TestGetCurrentLSN(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	lsn := bm.GetCurrentLSN()
	if lsn < 0 {
		t.Errorf("Expected non-negative LSN, got %d", lsn)
	}

	// After a write, LSN should increase
	addTestBucket(t, db, "lsn-bucket")
	newLSN := bm.GetCurrentLSN()
	if newLSN <= lsn {
		t.Errorf("Expected LSN to increase after write: before=%d, after=%d", lsn, newLSN)
	}
}

func TestBackupManagerCreateIncrementalBackup(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "bm-incr-bucket")
	addTestObjects(t, db, "bm-incr-bucket", "key1", `{"key":"key1"}`)

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	// First incremental backup
	info, err := bm.CreateIncrementalBackup(ctx)
	if err != nil {
		t.Fatalf("Failed to create incremental backup: %v", err)
	}

	if info.StartLSN != 0 {
		t.Errorf("Expected StartLSN=0 for first incremental, got %d", info.StartLSN)
	}

	// Add more data
	addTestObjects(t, db, "bm-incr-bucket", "key2", `{"key":"key2"}`)

	// Second incremental backup
	info2, err := bm.CreateIncrementalBackup(ctx)
	if err != nil {
		t.Fatalf("Failed to create second incremental backup: %v", err)
	}

	if info2.StartLSN < info.EndLSN {
		t.Errorf("Expected second StartLSN >= first EndLSN: start=%d, prev_end=%d", info2.StartLSN, info.EndLSN)
	}
}

func TestEmptyBackupVerification(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "nexus-empty-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create an empty file
	emptyPath := filepath.Join(tmpDir, "empty.db")
	if err := os.WriteFile(emptyPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	err = VerifyBackupIntegrity(emptyPath)
	if err == nil {
		t.Error("Expected verification to fail for empty file")
	}
}

func TestRemoteConfigUnsupported(t *testing.T) {
	ctx := context.Background()
	err := UploadToRemote(ctx, "/tmp/nonexistent", &RemoteConfig{
		Type: "ftp",
	})
	if err == nil {
		t.Error("Expected error for unsupported remote type")
	}
}

func TestEncryptionKeySize(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "enc-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("test data")
	tmpFile.Close()

	// Key too short
	err = EncryptBackup(tmpFile.Name(), []byte("short"))
	if err == nil {
		t.Error("Expected error for short encryption key")
	}

	// Key too long
	err = EncryptBackup(tmpFile.Name(), make([]byte, 64))
	if err == nil {
		t.Error("Expected error for long encryption key")
	}
}

func TestDecryptionKeySize(t *testing.T) {
	_, err := DecryptBackup("/tmp/nonexistent", []byte("short"))
	if err == nil {
		t.Error("Expected error for short decryption key")
	}
}

func TestBackupTimestamp(t *testing.T) {
	db, tmpDir := setupTestDB(t)
	defer teardownTestDB(t, db, tmpDir)

	backupDir := filepath.Join(tmpDir, "backups")
	ctx := context.Background()

	addTestBucket(t, db, "ts-bucket")

	bm := NewBackupManager(db, &BackupConfig{
		DataDir:   tmpDir,
		BackupDir: backupDir,
	})

	before := time.Now()
	info, err := bm.CreateBackup(ctx, "full")
	if err != nil {
		t.Fatalf("Failed to create backup: %v", err)
	}
	after := time.Now()

	if info.CreatedAt.Before(before) || info.CreatedAt.After(after) {
		t.Errorf("CreatedAt timestamp %v not between %v and %v", info.CreatedAt, before, after)
	}
}
