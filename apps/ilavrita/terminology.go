package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/Ilavrita/Ilavrita/packages/terminology"
	"github.com/pocketbase/pocketbase"
	"github.com/spf13/cobra"
)

// errMissingRelease reports an import naming files this build cannot find.
var errMissingRelease = errors.New("ilavrita: that release is not where the command says it is")

// registerTerminologyCommands publishes the commands a deployment loads its own
// code systems with.
//
// They are commands and not routes, and the content is the deployment's and not
// this project's. LOINC and SNOMED CT are licensed — SNOMED per country and per
// affiliate, and its use in a Member country still has to be registered with
// that country's National Release Center — so nothing here ships a release.
// What ships is the reading of one.
func registerTerminologyCommands(app *pocketbase.PocketBase) {
	held := &cobra.Command{
		Use:   "terminology",
		Short: "Load and inspect the code systems this install holds",
	}

	held.AddCommand(importLOINCCommand(app), importSNOMEDCommand(app), listTerminologyCommand(app))
	app.RootCmd.AddCommand(held)
}

// importLOINCCommand loads a LOINC table release.
func importLOINCCommand(app *pocketbase.PocketBase) *cobra.Command {
	var version string

	command := &cobra.Command{
		Use:   "import-loinc <Loinc.csv>",
		Short: "Load a LOINC table release from your own licensed copy",
		Long: "Reads the LOINC_NUM, LONG_COMMON_NAME and STATUS columns of a LOINC table " +
			"release. The file is yours: this project ships no LOINC content, and using " +
			"LOINC is subject to its own licence.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			held, err := os.Open(args[0]) //nolint:gosec // a path the operator typed.
			if err != nil {
				return fmt.Errorf("%w: %w", errMissingRelease, err)
			}

			defer func() { _ = held.Close() }()

			return importRelease(command, app,
				terminology.NewLOINCRelease(held, version), filepath.Base(args[0]))
		},
	}

	command.Flags().StringVar(&version, "version", "unstated",
		"which LOINC release this is, recorded so an operator can tell which one is loaded")

	return command
}

// importSNOMEDCommand loads a SNOMED CT RF2 snapshot.
func importSNOMEDCommand(app *pocketbase.PocketBase) *cobra.Command {
	var version string

	command := &cobra.Command{
		Use:   "import-snomed <sct2_Concept_Snapshot> <sct2_Description_Snapshot>",
		Short: "Load a SNOMED CT RF2 snapshot from your own licensed copy",
		Long: "Reads an RF2 snapshot's concept and description files. Both are needed: the " +
			"concept file says what exists, the description file says what it is called.\n\n" +
			"The release is yours. SNOMED CT is licensed per country and per affiliate, and " +
			"even free use in a Member country has to be registered with that country's " +
			"National Release Center. This project ships no SNOMED content.",
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			concepts, err := os.Open(args[0]) //nolint:gosec // a path the operator typed.
			if err != nil {
				return fmt.Errorf("%w: %w", errMissingRelease, err)
			}

			defer func() { _ = concepts.Close() }()

			descriptions, err := os.Open(args[1]) //nolint:gosec // as above.
			if err != nil {
				return fmt.Errorf("%w: %w", errMissingRelease, err)
			}

			defer func() { _ = descriptions.Close() }()

			return importRelease(command, app,
				terminology.NewSNOMEDRelease(concepts, descriptions, version),
				filepath.Base(args[0]))
		},
	}

	command.Flags().StringVar(&version, "version", "unstated",
		"which edition and effective date this is, recorded so an operator can tell")

	return command
}

// importRelease loads one release into the install.
func importRelease(
	command *cobra.Command, app *pocketbase.PocketBase, release terminology.Release, source string,
) error {
	db, err := openDatabase(app.DataDir())
	if err != nil {
		return err
	}

	defer func() { _ = db.Close() }()

	if err := prepareDatabase(command.Context(), db.DB()); err != nil {
		return err
	}

	loaded, err := sqlite.NewTerminologyStore(db.DB()).
		Import(command.Context(), release, source, time.Now())
	if err != nil {
		return err
	}

	command.Printf("loaded %d concepts of %s %s\n", loaded.Held, loaded.System, loaded.Version)

	return nil
}

// listTerminologyCommand reports what the install holds, which is what an
// operator asks before trusting a lookup.
func listTerminologyCommand(app *pocketbase.PocketBase) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Report which code systems this install holds",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			db, err := openDatabase(app.DataDir())
			if err != nil {
				return err
			}

			defer func() { _ = db.Close() }()

			if err := prepareDatabase(command.Context(), db.DB()); err != nil {
				return err
			}

			held, err := sqlite.NewTerminologyStore(db.DB()).Systems(command.Context())
			if err != nil {
				return err
			}

			if len(held) == 0 {
				command.Println("no code systems are loaded")

				return nil
			}

			for _, system := range held {
				command.Printf("%s  %s  %d concepts  from %s on %s\n",
					system.System, system.Version, system.Held, system.Source,
					system.ImportedAt.Format(time.RFC3339))
			}

			return nil
		},
	}
}
