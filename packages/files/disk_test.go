package files_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func aKey() files.Key {
	return files.Key{Project: "prj_a", Type: "Binary", ID: "bin-1", Version: "1"}
}

func newDisk(t *testing.T) (*files.Disk, string) {
	t.Helper()

	root := t.TempDir()

	return files.NewDisk(root), root
}

func put(t *testing.T, store *files.Disk, key files.Key, media, body string) files.Stored {
	t.Helper()

	stored, err := store.Put(context.Background(), key, media, strings.NewReader(body), 1<<20)
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	return stored
}

func read(t *testing.T, store *files.Disk, key files.Key) (string, files.Stored) {
	t.Helper()

	held, stored, err := store.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = held.Close() }()

	body, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(body), stored
}

// TestAPayloadComesBackAsItWasWritten, media type included: what a client
// called it is what this server hands back, rather than something guessed from
// the bytes.
func TestAPayloadComesBackAsItWasWritten(t *testing.T) {
	store, _ := newDisk(t)

	written := put(t, store, aKey(), "application/pdf", "%PDF-1.7 not really")

	body, stored := read(t, store, aKey())
	if body != "%PDF-1.7 not really" {
		t.Errorf("read back %q", body)
	}

	if stored.Media != "application/pdf" {
		t.Errorf("media is %q", stored.Media)
	}

	if stored.Size != written.Size || stored.Size != int64(len("%PDF-1.7 not really")) {
		t.Errorf("size is %d", stored.Size)
	}

	if stored.Digest != written.Digest || len(stored.Digest) != 64 {
		t.Errorf("digest is %q", stored.Digest)
	}
}

// TestEachVersionKeepsItsOwnBytes. An updated Binary must not overwrite what
// its earlier version served, or a history read answers with something that was
// never written then.
func TestEachVersionKeepsItsOwnBytes(t *testing.T) {
	store, _ := newDisk(t)

	first := aKey()
	second := aKey()
	second.Version = "2"

	put(t, store, first, "text/plain", "the first")
	put(t, store, second, "text/plain", "the second")

	if body, _ := read(t, store, first); body != "the first" {
		t.Errorf("version 1 reads %q", body)
	}

	if body, _ := read(t, store, second); body != "the second" {
		t.Errorf("version 2 reads %q", body)
	}
}

// TestAPayloadPastTheLimitIsRefusedWhileItIsRead, not after: a limit checked at
// the end is one the disk has already paid for.
func TestAPayloadPastTheLimitIsRefusedWhileItIsRead(t *testing.T) {
	store, root := newDisk(t)

	_, err := store.Put(context.Background(), aKey(), "text/plain",
		strings.NewReader(strings.Repeat("x", 100)), 10)
	if !errors.Is(err, files.ErrTooLarge) {
		t.Fatalf("err = %v, want %v", err, files.ErrTooLarge)
	}

	// Nothing was left behind, not even the partial write.
	var left []string

	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, _ error) error {
		if entry != nil && !entry.IsDir() {
			left = append(left, path)
		}

		return nil
	})

	if len(left) != 0 {
		t.Errorf("a refused payload left %v", left)
	}
}

// TestExactlyTheLimitIsAccepted, so the refusal above is the size and not an
// off-by-one that rejects what was allowed.
func TestExactlyTheLimitIsAccepted(t *testing.T) {
	store, _ := newDisk(t)

	stored, err := store.Put(context.Background(), aKey(), "text/plain",
		strings.NewReader(strings.Repeat("x", 10)), 10)
	if err != nil {
		t.Fatalf("a payload of exactly the limit was refused: %v", err)
	}

	if stored.Size != 10 {
		t.Errorf("size is %d", stored.Size)
	}
}

// TestAKeyCannotWalkOutOfTheRoot. A key is built from a Project and a resource
// id, and an id that could walk out would let one Project read another's
// documents.
func TestAKeyCannotWalkOutOfTheRoot(t *testing.T) {
	store, root := newDisk(t)

	escapes := map[string]files.Key{
		"a parent in the id":      {Project: "prj_a", Type: "Binary", ID: "..", Version: "1"},
		"a separator in the id":   {Project: "prj_a", Type: "Binary", ID: "../../etc/passwd", Version: "1"},
		"a separator in the type": {Project: "prj_a", Type: "../..", ID: "bin-1", Version: "1"},
		"a parent in the project": {Project: "..", Type: "Binary", ID: "bin-1", Version: "1"},
		"a separator in the version": {
			Project: "prj_a", Type: "Binary", ID: "bin-1", Version: "../1",
		},
		"a hidden segment": {Project: "prj_a", Type: "Binary", ID: ".ssh", Version: "1"},
		"a null byte":      {Project: "prj_a", Type: "Binary", ID: "bin\x001", Version: "1"},
		"a backslash":      {Project: "prj_a", Type: `..\..`, ID: "bin-1", Version: "1"},
		"nothing at all":   {},
		"no version":       {Project: "prj_a", Type: "Binary", ID: "bin-1"},
	}

	for name, key := range escapes {
		_, err := store.Put(context.Background(), key, "text/plain", strings.NewReader("x"), 1<<20)
		if !errors.Is(err, files.ErrMalformedKey) {
			t.Errorf("%s: err = %v, want %v", name, err, files.ErrMalformedKey)
		}

		if _, _, err := store.Open(context.Background(), key); !errors.Is(err, files.ErrMalformedKey) {
			t.Errorf("%s: open err = %v, want %v", name, err, files.ErrMalformedKey)
		}
	}

	// And nothing was written anywhere.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the root: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("a refused key wrote %d entries into the root", len(entries))
	}
}

// TestOneProjectsPayloadsAreNotAnothers.
func TestOneProjectsPayloadsAreNotAnothers(t *testing.T) {
	store, _ := newDisk(t)

	mine := aKey()
	theirs := aKey()
	theirs.Project = "prj_b"

	put(t, store, mine, "text/plain", "mine")

	if _, _, err := store.Open(context.Background(), theirs); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("another Project read it: %v", err)
	}
}

// TestAPayloadThatIsNotThereIsNotFound, which a route answers the way it
// answers a missing resource.
func TestAPayloadThatIsNotThereIsNotFound(t *testing.T) {
	store, _ := newDisk(t)

	if _, _, err := store.Open(context.Background(), aKey()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("err = %v, want %v", err, files.ErrNotFound)
	}

	if _, err := store.Describe(context.Background(), aKey()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("describe err = %v, want %v", err, files.ErrNotFound)
	}
}

// TestRemovingTakesEveryVersion, which is what a Project's purge needs: a
// resource's payloads are its history, and half of them left behind is a
// document nobody can account for.
func TestRemovingTakesEveryVersion(t *testing.T) {
	store, _ := newDisk(t)

	for _, version := range []storage.VersionID{"1", "2", "3"} {
		key := aKey()
		key.Version = version
		put(t, store, key, "text/plain", "version "+string(version))
	}

	if err := store.Remove(context.Background(), "prj_a", "Binary", "bin-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	for _, version := range []storage.VersionID{"1", "2", "3"} {
		key := aKey()
		key.Version = version

		if _, _, err := store.Open(context.Background(), key); !errors.Is(err, files.ErrNotFound) {
			t.Errorf("version %s survived: %v", version, err)
		}
	}
}

// TestAPayloadIsNotWorldReadable. These are patient documents, and a
// world-readable file is one a backup agent, a sidecar or a shell on the same
// host can take.
func TestAPayloadIsNotWorldReadable(t *testing.T) {
	store, root := newDisk(t)

	put(t, store, aKey(), "text/plain", "private")

	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, _ error) error {
		if entry == nil || path == root {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return nil
		}

		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v", path, info.Mode().Perm())
		}

		return nil
	})
}

// TestAHalfWrittenPayloadIsNeverServed. A read must not see a file something is
// still writing, so the bytes are placed under their name only once they are
// all there.
func TestAHalfWrittenPayloadIsNeverServed(t *testing.T) {
	store, _ := newDisk(t)

	// A reader that fails partway is what a dropped connection looks like.
	failing := io.MultiReader(strings.NewReader("the beginning"), errorReader{})

	if _, err := store.Put(context.Background(), aKey(), "text/plain", failing, 1<<20); err == nil {
		t.Fatal("a payload that could not be read was accepted")
	}

	if _, _, err := store.Open(context.Background(), aKey()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a half-written payload is readable: %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("the connection dropped") }

// TestTheDigestIsOfTheBytesThemselves, so what it says about a payload is
// checkable rather than asserted.
func TestTheDigestIsOfTheBytesThemselves(t *testing.T) {
	store, _ := newDisk(t)

	first := put(t, store, aKey(), "text/plain", "identical")

	second := aKey()
	second.Version = "2"

	if again := put(t, store, second, "text/plain", "identical"); again.Digest != first.Digest {
		t.Error("the same bytes digested differently")
	}

	third := aKey()
	third.Version = "3"

	if other := put(t, store, third, "text/plain", "different"); other.Digest == first.Digest {
		t.Error("different bytes digested the same")
	}
}

// countingReader reports how much of a body was actually asked for.
type countingReader struct {
	body io.Reader
	read int64
}

func (c *countingReader) Read(into []byte) (int, error) {
	held, err := c.body.Read(into)
	c.read += int64(held)

	return held, err
}

// TestAnOversizedPayloadIsNotReadPastTheLimit. Refusing after reading it all is
// a limit the disk and the connection have already paid for, which is most of
// what the limit exists to prevent.
func TestAnOversizedPayloadIsNotReadPastTheLimit(t *testing.T) {
	store, _ := newDisk(t)

	const limit = 10

	counted := &countingReader{body: strings.NewReader(strings.Repeat("x", 1<<20))}

	if _, err := store.Put(context.Background(), aKey(), "text/plain", counted, limit); !errors.Is(err, files.ErrTooLarge) {
		t.Fatalf("err = %v, want %v", err, files.ErrTooLarge)
	}

	// One byte past the limit is what says it was exceeded; anything more than
	// that was read for nothing.
	if counted.read > limit+1 {
		t.Errorf("%d bytes were read to refuse a %d-byte limit", counted.read, limit)
	}
}

// TestDiscardingTakesOneVersionAndLeavesTheRest. It is what a write that did
// not commit calls, and a write that did not commit is one version of one
// resource — not the history beside it.
func TestDiscardingTakesOneVersionAndLeavesTheRest(t *testing.T) {
	store, _ := newDisk(t)

	first := aKey()
	put(t, store, first, "text/plain", "the first")

	second := aKey()
	second.Version = "2"
	put(t, store, second, "text/plain", "the second")

	if err := store.Discard(context.Background(), second); err != nil {
		t.Fatalf("discard: %v", err)
	}

	if _, _, err := store.Open(context.Background(), second); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a discarded payload is still readable: %v", err)
	}

	if body, _ := read(t, store, first); body != "the first" {
		t.Errorf("the version beside it reads %q", body)
	}
}

// TestDiscardingLeavesNothingBehindOnTheDisk. The point of taking a payload
// back is that the document is gone, not that it stops being served: a file
// under a name nothing reads is still a patient document on the disk.
func TestDiscardingLeavesNothingBehindOnTheDisk(t *testing.T) {
	store, root := newDisk(t)

	put(t, store, aKey(), "text/plain", "a document")

	if err := store.Discard(context.Background(), aKey()); err != nil {
		t.Fatalf("discard: %v", err)
	}

	var left []string

	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() {
			left = append(left, path)
		}

		return nil
	}); err != nil {
		t.Fatalf("walk the store: %v", err)
	}

	if len(left) != 0 {
		t.Errorf("the discard left %v behind", left)
	}
}

// TestDiscardingWhatIsNotThereIsQuiet. A write rolls back whether or not it
// reached the disk, so the compensation has to be safe to run either way — and
// safe to run twice, because two boundaries may both take it back.
func TestDiscardingWhatIsNotThereIsQuiet(t *testing.T) {
	store, _ := newDisk(t)

	put(t, store, aKey(), "text/plain", "a document")

	for attempt := range 3 {
		if err := store.Discard(context.Background(), aKey()); err != nil {
			t.Fatalf("discard %d: %v", attempt, err)
		}
	}
}

// TestDiscardingRefusesAKeyThatCouldWalkOutOfTheRoot, the same as every other
// route into this store: a removal that escaped would delete another Project's
// documents rather than read them.
func TestDiscardingRefusesAKeyThatCouldWalkOutOfTheRoot(t *testing.T) {
	store, _ := newDisk(t)

	for _, id := range []string{"..", "../../etc", "a/b", ""} {
		key := aKey()
		key.ID = storage.LogicalID(id)

		if err := store.Discard(context.Background(), key); !errors.Is(err, files.ErrMalformedKey) {
			t.Errorf("discarding %q answered %v", id, err)
		}
	}
}

// TestAPayloadIsVisibleOnlyOnceItIsDescribed. The two files are placed in that
// order on purpose: a crash between them leaves bytes nothing names, which is
// wasted space, rather than a name with nothing behind it, which is a read that
// fails after saying the payload is there.
func TestAPayloadIsVisibleOnlyOnceItIsDescribed(t *testing.T) {
	store, root := newDisk(t)

	put(t, store, aKey(), "text/plain", "a document")

	described := filepath.Join(root, "prj_a", "Binary", "bin-1", "1.meta")
	if err := os.Remove(described); err != nil {
		t.Fatalf("remove what says what the payload is: %v", err)
	}

	if _, err := store.Describe(context.Background(), aKey()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a payload nothing describes is described as %v", err)
	}

	if _, _, err := store.Open(context.Background(), aKey()); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("a payload nothing describes is readable: %v", err)
	}
}
