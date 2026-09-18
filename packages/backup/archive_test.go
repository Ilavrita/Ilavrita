package backup_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/backup"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"

	_ "modernc.org/sqlite"
)

// takenAt is when every archive below says it was written.
var takenAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// anInstall builds a data directory holding a database with rows in it and a
// payload beside them, which is what an install is.
func anInstall(t *testing.T) string {
	t.Helper()

	dataDir := t.TempDir()

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, backup.DatabaseFile))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := db.Exec(
		"CREATE TABLE fhir_resource (id TEXT PRIMARY KEY, content TEXT)"); err != nil {
		t.Fatalf("declare: %v", err)
	}

	if _, err := db.Exec(
		"INSERT INTO fhir_resource VALUES ('bin-1', '{\"resourceType\":\"Binary\"}')"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	writePayload(t, filepath.Join(dataDir, "payloads", "prj_a", "Binary", "bin-1"), "1", "a document")

	return dataDir
}

func writePayload(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make room: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write a payload: %v", err)
	}
}

// taken writes an archive of one install.
func taken(t *testing.T, dataDir string) string {
	t.Helper()

	archive := filepath.Join(t.TempDir(), "archive")

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, backup.DatabaseFile))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}

	defer func() { _ = db.Close() }()

	if _, err := backup.Take(archive, sqlite.NewSnapshot(db),
		filepath.Join(dataDir, "payloads"), takenAt); err != nil {
		t.Fatalf("take a backup: %v", err)
	}

	return archive
}

// TestAnArchiveHoldsBothStores. An install is a database and the payloads its
// rows describe; a copy of one without the other is not a backup of anything.
func TestAnArchiveHoldsBothStores(t *testing.T) {
	manifest, err := backup.Verify(taken(t, anInstall(t)))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	if !manifest.TakenAt.Equal(takenAt) {
		t.Errorf("the archive says it was taken at %s", manifest.TakenAt)
	}

	for _, named := range []string{
		backup.DatabaseFile, "payloads/prj_a/Binary/bin-1/1",
	} {
		if _, held := manifest.Files[named]; !held {
			t.Errorf("the archive holds no %s: it holds %v", named, manifest.Files)
		}
	}
}

// TestARestoredInstallIsTheOneThatWasTakenFrom, rows and bytes alike.
func TestARestoredInstallIsTheOneThatWasTakenFrom(t *testing.T) {
	archive := taken(t, anInstall(t))
	into := filepath.Join(t.TempDir(), "restored")

	if _, err := backup.Restore(archive, into); err != nil {
		t.Fatalf("restore: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(into, backup.DatabaseFile))
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}

	defer func() { _ = db.Close() }()

	var content string
	if err := db.QueryRow("SELECT content FROM fhir_resource WHERE id = 'bin-1'").
		Scan(&content); err != nil {
		t.Fatalf("read the restored row: %v", err)
	}

	if content != `{"resourceType":"Binary"}` {
		t.Errorf("the restored row holds %q", content)
	}

	body, err := os.ReadFile( //nolint:gosec // a path this test made under its own temporary directory.
		filepath.Join(into, "payloads", "prj_a", "Binary", "bin-1", "1"))
	if err != nil {
		t.Fatalf("read the restored payload: %v", err)
	}

	if string(body) != "a document" {
		t.Errorf("the restored payload holds %q", body)
	}
}

// TestAnArchiveThatWasTamperedWithIsRefused. A backup nobody checked is one
// whose first test is the day it is needed, so every file is compared with what
// the manifest says it was written as.
func TestAnArchiveThatWasTamperedWithIsRefused(t *testing.T) {
	for described, damage := range map[string]func(t *testing.T, archive string){
		"a payload that changed": func(t *testing.T, archive string) {
			t.Helper()
			writePayload(t, filepath.Join(archive, "payloads", "prj_a", "Binary", "bin-1"),
				"1", "a different document")
		},
		// The same length as what was written, so nothing but the digest can
		// tell. A tamper that kept the size is the one a size check misses.
		"a payload swapped for another of the same length": func(t *testing.T, archive string) {
			t.Helper()
			writePayload(t, filepath.Join(archive, "payloads", "prj_a", "Binary", "bin-1"),
				"1", "A DOCUMENT")
		},
		"a database that changed": func(t *testing.T, archive string) {
			t.Helper()

			if err := os.WriteFile(filepath.Join(archive, backup.DatabaseFile),
				[]byte("not a database"), 0o600); err != nil {
				t.Fatalf("damage the database: %v", err)
			}
		},
		"a file that is gone": func(t *testing.T, archive string) {
			t.Helper()

			if err := os.Remove(filepath.Join(archive,
				"payloads", "prj_a", "Binary", "bin-1", "1")); err != nil {
				t.Fatalf("take a file away: %v", err)
			}
		},
	} {
		archive := taken(t, anInstall(t))
		damage(t, archive)

		if _, err := backup.Verify(archive); !errors.Is(err, backup.ErrArchiveIncomplete) {
			t.Errorf("%s verified as %v", described, err)
		}

		if _, err := backup.Restore(archive, filepath.Join(t.TempDir(), "restored")); err == nil {
			t.Errorf("%s was restored", described)
		}
	}
}

// TestARestoreLeavesALiveInstallAlone. Restoring over one is a thing to do
// deliberately, with the old one moved aside by somebody who knows they mean it.
func TestARestoreLeavesALiveInstallAlone(t *testing.T) {
	archive := taken(t, anInstall(t))
	live := anInstall(t)

	if _, err := backup.Restore(archive, live); !errors.Is(err, backup.ErrOccupied) {
		t.Errorf("a restore over a live install answered %v", err)
	}
}

// TestADirectoryThatIsNotAnArchiveIsRefused, rather than restored as far as it
// goes. An archive whose manifest was never written is one that was interrupted.
func TestADirectoryThatIsNotAnArchiveIsRefused(t *testing.T) {
	empty := t.TempDir()

	if _, err := backup.Verify(empty); !errors.Is(err, backup.ErrNotAnArchive) {
		t.Errorf("an empty directory verified as %v", err)
	}

	// An archive whose manifest was removed: every file is there, and nothing
	// says the set is complete.
	archive := taken(t, anInstall(t))

	if err := os.Remove(filepath.Join(archive, backup.ManifestFile)); err != nil {
		t.Fatalf("take the manifest away: %v", err)
	}

	if _, err := backup.Restore(archive, filepath.Join(t.TempDir(), "restored")); !errors.Is(
		err, backup.ErrNotAnArchive) {
		t.Errorf("an archive with no manifest answered %v", err)
	}
}

// TestAManifestCannotNameSomewhereElse. A manifest is a file, and a file is
// something somebody can edit: one naming a path outside the archive must not
// be followed, in either direction.
func TestAManifestCannotNameSomewhereElse(t *testing.T) {
	archive := taken(t, anInstall(t))

	body, err := os.ReadFile( //nolint:gosec // as above.
		filepath.Join(archive, backup.ManifestFile))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	var manifest backup.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("read the manifest: %v", err)
	}

	manifest.Files["../../../etc/ilavrita-probe"] = backup.Recorded{Digest: "0", Size: 0}

	edited, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("edit the manifest: %v", err)
	}

	if err := os.WriteFile(filepath.Join(archive, backup.ManifestFile), edited, 0o600); err != nil {
		t.Fatalf("write the edited manifest: %v", err)
	}

	if _, err := backup.Verify(archive); !errors.Is(err, backup.ErrUnsafePath) {
		t.Errorf("a manifest naming somewhere else answered %v", err)
	}
}

// TestAnInstallWithNoPayloadsBacksUpAnyway, which is what a deployment that has
// stored no Binary has.
func TestAnInstallWithNoPayloadsBacksUpAnyway(t *testing.T) {
	dataDir := t.TempDir()

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, backup.DatabaseFile))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if _, err := db.Exec("CREATE TABLE fhir_resource (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("declare: %v", err)
	}

	_ = db.Close()

	manifest, err := backup.Verify(taken(t, dataDir))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	if len(manifest.Files) != 1 {
		t.Errorf("an install with no payloads archived %v", manifest.Files)
	}
}

// TestAnArchiveIsNotReadableByAnybodyElse. It holds everything the database
// holds, and it is a thing an operator moves between machines — including onto
// ones where a backup agent or a shell sees whatever is world-readable.
func TestAnArchiveIsNotReadableByAnybodyElse(t *testing.T) {
	archive := taken(t, anInstall(t))

	manifest, err := backup.Read(archive)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	named := []string{backup.ManifestFile}
	for path := range manifest.Files {
		named = append(named, path)
	}

	for _, path := range named {
		held, err := os.Stat(filepath.Join(archive, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("look at %s: %v", path, err)
		}

		if mode := held.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is %04o, which somebody else can read", path, mode)
		}
	}
}
