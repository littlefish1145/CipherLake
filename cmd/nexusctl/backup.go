package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.etcd.io/bbolt"
	"nexus/internal/backup"
)

var (
	backupDataDir string
	backupDir     string
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Backup management commands",
	Long:  "Create, list, restore, verify, and drill backup operations for Nexus storage",
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all backups (full + incremental)",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := getBackupDir()
		db := getBackupDB(dir)
		if db != nil {
			defer db.Close()
		}

		bm := backup.NewBackupManager(db, &backup.BackupConfig{
			DataDir:   getDataDir(),
			BackupDir: dir,
		})

		fullBackups, incrBackups, err := bm.ListAllBackups(cmd.Context())
		if err != nil {
			return fmt.Errorf("failed to list backups: %w", err)
		}

		result := map[string]interface{}{
			"full":        formatBackupList(fullBackups),
			"incremental": formatIncrementalList(incrBackups),
			"total_full":  len(fullBackups),
			"total_incr":  len(incrBackups),
		}

		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

var backupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a full or incremental backup",
	RunE: func(cmd *cobra.Command, args []string) error {
		incremental, _ := cmd.Flags().GetBool("incremental")
		dir := getBackupDir()
		db := getBackupDB(dir)
		if db != nil {
			defer db.Close()
		}

		bm := backup.NewBackupManager(db, &backup.BackupConfig{
			DataDir:   getDataDir(),
			BackupDir: dir,
		})

		if incremental {
			info, err := bm.CreateIncrementalBackup(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to create incremental backup: %w", err)
			}
			result := map[string]interface{}{
				"type":        "incremental",
				"name":        info.Name,
				"path":        info.Path,
				"size":        info.Size,
				"start_lsn":   info.StartLSN,
				"end_lsn":     info.EndLSN,
				"checksum":    info.Checksum,
				"object_count": info.ObjectCount,
				"created_at":  info.CreatedAt.Format(time.RFC3339),
			}
			out, err := formatOutput(result, outputFmt, queryStr)
			if err != nil {
				return err
			}
			fmt.Println(out)
		} else {
			info, err := bm.CreateBackup(cmd.Context(), "manual")
			if err != nil {
				return fmt.Errorf("failed to create backup: %w", err)
			}
			result := map[string]interface{}{
				"type":       "full",
				"name":       info.Name,
				"path":       info.Path,
				"size":       info.Size,
				"created_at": info.CreatedAt.Format(time.RFC3339),
			}
			out, err := formatOutput(result, outputFmt, queryStr)
			if err != nil {
				return err
			}
			fmt.Println(out)
		}
		return nil
	},
}

var backupRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore from backup",
	Long:  "Restore from a full backup, or from multiple backups (full + incremental). Apply full backup first, then incremental backups in order.",
	RunE: func(cmd *cobra.Command, args []string) error {
		fromPaths, _ := cmd.Flags().GetStringSlice("from")
		if len(fromPaths) == 0 {
			return fmt.Errorf("--from flag is required with at least one backup path")
		}

		dir := getBackupDir()
		db := getBackupDB(dir)
		if db != nil {
			defer db.Close()
		}

		bm := backup.NewBackupManager(db, &backup.BackupConfig{
			DataDir:   getDataDir(),
			BackupDir: dir,
		})

		if err := bm.RestoreFromBackup(cmd.Context(), fromPaths); err != nil {
			return fmt.Errorf("failed to restore: %w", err)
		}

		result := map[string]interface{}{
			"message":       "Restore completed successfully",
			"backup_count":  len(fromPaths),
			"backup_paths":  fromPaths,
		}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

var backupVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify backup integrity",
	Long:  "Verify that a backup file can be read, parsed, and its checksum matches the manifest",
	RunE: func(cmd *cobra.Command, args []string) error {
		fromPath, _ := cmd.Flags().GetString("from")
		if fromPath == "" {
			return fmt.Errorf("--from flag is required")
		}

		err := backup.VerifyBackupIntegrity(fromPath)
		if err != nil {
			result := map[string]interface{}{
				"valid":  false,
				"path":   fromPath,
				"error":  err.Error(),
			}
			out, _ := formatOutput(result, outputFmt, queryStr)
			fmt.Println(out)
			return fmt.Errorf("backup verification failed: %w", err)
		}

		result := map[string]interface{}{
			"valid": true,
			"path":  fromPath,
		}
		out, err := formatOutput(result, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	},
}

var backupDrillCmd = &cobra.Command{
	Use:   "drill",
	Short: "Perform a backup drill test",
	Long:  "Start a temporary Nexus instance, feed backup data into it, and verify it can start, read objects, and list buckets",
	RunE: func(cmd *cobra.Command, args []string) error {
		fromPath, _ := cmd.Flags().GetString("from")
		if fromPath == "" {
			return fmt.Errorf("--from flag is required")
		}

		dir := getBackupDir()
		db := getBackupDB(dir)
		if db != nil {
			defer db.Close()
		}

		bm := backup.NewBackupManager(db, &backup.BackupConfig{
			DataDir:   getDataDir(),
			BackupDir: dir,
		})

		drillResult, err := bm.DrillBackup(cmd.Context(), fromPath)
		if err != nil {
			return fmt.Errorf("drill failed: %w", err)
		}

		out, err := formatOutput(drillResult, outputFmt, queryStr)
		if err != nil {
			return err
		}
		fmt.Println(out)

		if !drillResult.Success {
			os.Exit(1)
		}
		return nil
	},
}

func getBackupDir() string {
	if backupDir != "" {
		return backupDir
	}
	return filepath.Join(getDataDir(), "backups")
}

func getDataDir() string {
	if backupDataDir != "" {
		return backupDataDir
	}
	return "data"
}

func getBackupDB(backupDir string) *bbolt.DB {
	dataDir := getDataDir()
	dbPath := filepath.Join(dataDir, "metadata.db")
	db, err := bbolt.Open(dbPath, 0666, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not open database at %s: %v\n", dbPath, err)
		return nil
	}
	return db
}

func formatBackupList(backups []*backup.BackupInfo) []map[string]interface{} {
	var result []map[string]interface{}
	for _, b := range backups {
		result = append(result, map[string]interface{}{
			"name":       b.Name,
			"path":       b.Path,
			"size":       b.Size,
			"type":       b.Type,
			"created_at": b.CreatedAt.Format(time.RFC3339),
		})
	}
	return result
}

func formatIncrementalList(backups []*backup.IncrementalBackupInfo) []map[string]interface{} {
	var result []map[string]interface{}
	for _, b := range backups {
		result = append(result, map[string]interface{}{
			"name":         b.Name,
			"path":         b.Path,
			"size":         b.Size,
			"start_lsn":    b.StartLSN,
			"end_lsn":      b.EndLSN,
			"checksum":     b.Checksum,
			"object_count": b.ObjectCount,
			"created_at":   b.CreatedAt.Format(time.RFC3339),
		})
	}
	return result
}

// parseBackupPaths parses a comma-separated list of backup paths.
func parseBackupPaths(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var paths []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// marshalDrillResult marshals a drill result to JSON.
func marshalDrillResult(r *backup.DrillResult) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

func init() {
	rootCmd.AddCommand(backupCmd)

	backupCmd.AddCommand(backupListCmd)
	backupListCmd.Flags().StringVar(&backupDir, "backup-dir", "", "Backup directory path")
	backupListCmd.Flags().StringVar(&backupDataDir, "data-dir", "", "Data directory path")

	backupCmd.AddCommand(backupCreateCmd)
	backupCreateCmd.Flags().Bool("incremental", false, "Create an incremental backup instead of full")
	backupCreateCmd.Flags().StringVar(&backupDir, "backup-dir", "", "Backup directory path")
	backupCreateCmd.Flags().StringVar(&backupDataDir, "data-dir", "", "Data directory path")

	backupCmd.AddCommand(backupRestoreCmd)
	backupRestoreCmd.Flags().StringSlice("from", nil, "Backup path(s) to restore from (comma-separated or multiple --from flags). First is full, rest are incremental.")
	backupRestoreCmd.Flags().StringVar(&backupDir, "backup-dir", "", "Backup directory path")
	backupRestoreCmd.Flags().StringVar(&backupDataDir, "data-dir", "", "Data directory path")

	backupCmd.AddCommand(backupVerifyCmd)
	backupVerifyCmd.Flags().String("from", "", "Path to backup file to verify")
	backupVerifyCmd.MarkFlagRequired("from")

	backupCmd.AddCommand(backupDrillCmd)
	backupDrillCmd.Flags().String("from", "", "Path to backup file to drill")
	backupDrillCmd.MarkFlagRequired("from")
	backupDrillCmd.Flags().StringVar(&backupDir, "backup-dir", "", "Backup directory path")
	backupDrillCmd.Flags().StringVar(&backupDataDir, "data-dir", "", "Data directory path")
}
