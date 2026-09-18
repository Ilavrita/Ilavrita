package main

import (
	"path/filepath"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/backup"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase"
	"github.com/spf13/cobra"
)

// The commands an operator takes and puts back an install with.
//
// They are commands rather than routes. An archive holds everything the
// database holds — patient data, password hashes, sealed second factors and the
// audit trail — so it belongs on the operator's own filesystem rather than
// coming back over HTTP, and taking one is not something a bearer token should
// be able to do.
func registerBackupCommands(app *pocketbase.PocketBase) {
	app.RootCmd.AddCommand(backupCommand(app))
	app.RootCmd.AddCommand(restoreCommand(app))
	app.RootCmd.AddCommand(verifyBackupCommand())
}

func backupCommand(app *pocketbase.PocketBase) *cobra.Command {
	return &cobra.Command{
		Use:   "backup <directory>",
		Short: "Write an archive of this install's database and payloads",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return takeBackup(command, app.DataDir(), args[0])
		},
	}
}

// takeBackup snapshots the database and copies the payloads beside it.
func takeBackup(command *cobra.Command, dataDir, into string) error {
	db, err := openDatabase(dataDir)
	if err != nil {
		return err
	}

	defer func() { _ = db.Close() }()

	manifest, err := backup.Take(into, sqlite.NewSnapshot(db.DB()),
		filepath.Join(dataDir, payloadDirectory), time.Now())
	if err != nil {
		return err
	}

	command.Printf("wrote %d files to %s\n", len(manifest.Files), into)

	return nil
}

func restoreCommand(app *pocketbase.PocketBase) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <directory>",
		Short: "Put an archive back into this install's data directory",
		Long: "The data directory must hold no database. Restoring over a live install is " +
			"deliberate work: move the old one aside first.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			manifest, err := backup.Restore(args[0], app.DataDir())
			if err != nil {
				return err
			}

			command.Printf("restored %d files taken at %s\n",
				len(manifest.Files), manifest.TakenAt.Format(time.RFC3339))

			return nil
		},
	}
}

func verifyBackupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "verify-backup <directory>",
		Short: "Check an archive against its own manifest without restoring it",
		Long: "A backup nobody checked is one whose first test is the day it is needed. " +
			"This reads every file and compares it with what the manifest says it was.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			manifest, err := backup.Verify(args[0])
			if err != nil {
				return err
			}

			command.Printf("%d files, taken at %s, all as written\n",
				len(manifest.Files), manifest.TakenAt.Format(time.RFC3339))

			return nil
		},
	}
}
