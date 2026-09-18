package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// The permissions a payload and its directories are written with. Nothing but
// the process that wrote them needs to read them: these are patient documents,
// and a world-readable file is one a backup agent, a sidecar or a shell on the
// same host can take.
const (
	payloadMode   os.FileMode = 0o600
	directoryMode os.FileMode = 0o700
)

// describedSuffix names the file beside a payload that says what it is. The
// media type is a fact about the payload and belongs with it, so a store moved
// to another host carries it rather than depending on a database to explain it.
const describedSuffix = ".meta"

// Disk keeps payloads as files under one root.
//
// Every key becomes a path, and every path segment is checked rather than
// trusted: a key is built from a resource id and a Project, and an id that
// could walk out of the root would let one Project read another's documents.
type Disk struct {
	root string
}

var _ Store = (*Disk)(nil)

// NewDisk binds a store to a directory.
func NewDisk(root string) *Disk {
	return &Disk{root: root}
}

// Put writes one payload and what it is.
//
// The bytes land in a temporary file and are renamed into place, so a read
// never sees a half-written payload and a crash leaves no partial file under a
// name something will later serve.
//
// Both files are flushed to the disk before this returns, and the directory
// entries with them. That is what orders this store against the database: the
// caller places a payload inside the transaction that writes the row, so bytes
// durable before that commit mean a crash can leave a file no row names — which
// is wasted space — but never a row naming bytes that are not there. It does not
// defeat a drive that lies about its own cache, which is the caveat the database
// carries too.
func (d *Disk) Put(
	ctx context.Context, key Key, media string, body io.Reader, limit int64,
) (Stored, error) {
	path, err := d.pathFor(key)
	if err != nil {
		return Stored{}, err
	}

	if err := os.MkdirAll(filepath.Dir(path), directoryMode); err != nil {
		return Stored{}, fmt.Errorf("files: make room for a payload: %w", err)
	}

	var stored Stored

	if err := placed(path, func(into *os.File) error {
		read, err := copyInto(into, body, media, limit)
		stored = read

		return err
	}); err != nil {
		return Stored{}, err
	}

	described, err := json.Marshal(stored)
	if err != nil {
		return Stored{}, fmt.Errorf("files: describe a payload: %w", err)
	}

	// The sidecar is placed second because it is what makes the payload
	// visible: every read goes through it. A crash between the two leaves bytes
	// nothing names rather than a name with nothing behind it.
	if err := placed(path+describedSuffix, func(into *os.File) error {
		_, err := into.Write(described)

		return err
	}); err != nil {
		return Stored{}, err
	}

	_ = ctx

	return stored, nil
}

// placed writes one file and renames it into place, with the bytes and the
// directory entry both on the disk before it returns.
func placed(path string, write func(*os.File) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".writing-*")
	if err != nil {
		return fmt.Errorf("files: open a payload for writing: %w", err)
	}

	// Removed on every path that does not rename it into place.
	defer func() { _ = os.Remove(temporary.Name()) }()

	if err := write(temporary); err != nil {
		_ = temporary.Close()

		return err
	}

	if err := temporary.Chmod(payloadMode); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("files: restrict a payload: %w", err)
	}

	// Flushed before the rename, so the name never arrives ahead of the bytes.
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()

		return fmt.Errorf("files: flush a payload: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("files: finish writing a payload: %w", err)
	}

	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("files: place a payload: %w", err)
	}

	// A rename is a change to the directory, so without this the file is
	// durable under no name at all.
	return syncDirectory(filepath.Dir(path))
}

// syncDirectory flushes a directory's own entries.
func syncDirectory(path string) error {
	held, err := os.Open(path) //nolint:gosec // the caller built this path from a checked key.
	if err != nil {
		return fmt.Errorf("files: open a payload directory: %w", err)
	}

	if err := held.Sync(); err != nil {
		_ = held.Close()

		return fmt.Errorf("files: flush a payload directory: %w", err)
	}

	if err := held.Close(); err != nil {
		return fmt.Errorf("files: close a payload directory: %w", err)
	}

	return nil
}

// Discard removes one version's payload.
//
// It is what a write that did not commit calls. The bytes are placed inside the
// transaction that writes the row, so a transaction that rolls away leaves a
// document on the disk that the client was told was not stored.
func (d *Disk) Discard(_ context.Context, key Key) error {
	path, err := d.pathFor(key)
	if err != nil {
		return err
	}

	removed := false

	// The sidecar first, because it is what makes the payload visible: an
	// interrupted discard leaves bytes nothing names rather than a name with
	// nothing behind it.
	for _, name := range []string{path + describedSuffix, path} {
		switch err := os.Remove(name); {
		case err == nil:
			removed = true
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("files: discard a payload: %w", err)
		}
	}

	if !removed {
		return nil
	}

	return syncDirectory(filepath.Dir(path))
}

// copyInto streams the body onto disk, digesting as it goes and stopping the
// moment it is too large. Reading it all first would mean the limit is a thing
// the disk has already paid for.
func copyInto(into io.Writer, body io.Reader, media string, limit int64) (Stored, error) {
	digest := sha256.New()

	// One byte past the limit is what says it was exceeded rather than exactly
	// reached.
	written, err := io.Copy(io.MultiWriter(into, digest), io.LimitReader(body, limit+1))
	if err != nil {
		return Stored{}, fmt.Errorf("files: read a payload: %w", err)
	}

	if written > limit {
		return Stored{}, ErrTooLarge
	}

	return Stored{Media: media, Size: written, Digest: hex.EncodeToString(digest.Sum(nil))}, nil
}

// Open returns a payload's bytes and what it is.
func (d *Disk) Open(ctx context.Context, key Key) (io.ReadCloser, Stored, error) {
	stored, err := d.Describe(ctx, key)
	if err != nil {
		return nil, Stored{}, err
	}

	path, err := d.pathFor(key)
	if err != nil {
		return nil, Stored{}, err
	}

	held, err := os.Open(path) //nolint:gosec // pathFor refuses anything outside the root.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Stored{}, ErrNotFound
		}

		return nil, Stored{}, fmt.Errorf("files: open a payload: %w", err)
	}

	return held, stored, nil
}

// Describe reports what a payload is without reading it.
func (d *Disk) Describe(_ context.Context, key Key) (Stored, error) {
	path, err := d.pathFor(key)
	if err != nil {
		return Stored{}, err
	}

	body, err := os.ReadFile(path + describedSuffix) //nolint:gosec // pathFor refuses anything outside the root.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Stored{}, ErrNotFound
		}

		return Stored{}, fmt.Errorf("files: read what a payload is: %w", err)
	}

	var stored Stored
	if err := json.Unmarshal(body, &stored); err != nil {
		return Stored{}, fmt.Errorf("files: read what a payload is: %w", err)
	}

	return stored, nil
}

// Remove deletes every version of one resource's payload.
func (d *Disk) Remove(
	_ context.Context,
	held storage.ProjectID,
	resourceType storage.ResourceType,
	id storage.LogicalID,
) error {
	path, err := d.directoryFor(held, resourceType, id)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("files: remove a resource's payloads: %w", err)
	}

	return nil
}

// pathFor turns a key into the file it names.
func (d *Disk) pathFor(key Key) (string, error) {
	if !key.Valid() {
		return "", fmt.Errorf("%w: %+v", ErrMalformedKey, key)
	}

	directory, err := d.directoryFor(key.Project, key.Type, key.ID)
	if err != nil {
		return "", err
	}

	version, err := safeSegment(string(key.Version))
	if err != nil {
		return "", err
	}

	return filepath.Join(directory, version), nil
}

// directoryFor is where one resource's payloads live.
func (d *Disk) directoryFor(
	held storage.ProjectID, resourceType storage.ResourceType, id storage.LogicalID,
) (string, error) {
	segments := make([]string, 0, 4)
	segments = append(segments, d.root)

	for _, raw := range []string{string(held), string(resourceType), string(id)} {
		safe, err := safeSegment(raw)
		if err != nil {
			return "", err
		}

		segments = append(segments, safe)
	}

	return filepath.Join(segments...), nil
}

// safeSegment refuses anything that is not one path segment.
//
// A key is built from a Project and a resource id, and an id that could walk
// out of the root would let one Project read another's documents. Nothing is
// escaped or cleaned here: a segment that needed cleaning is one nobody meant
// to write, and cleaning it would answer a different question from the one
// asked.
func safeSegment(raw string) (string, error) {
	// A leading dot covers "." and ".." along with every hidden name, so a
	// segment that could name a parent or a dotfile is one refusal rather than
	// three overlapping ones.
	if raw == "" || strings.HasPrefix(raw, ".") {
		return "", fmt.Errorf("%w: %q", ErrMalformedKey, raw)
	}

	if strings.ContainsAny(raw, `/\`) || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: %q", ErrMalformedKey, raw)
	}

	return raw, nil
}
