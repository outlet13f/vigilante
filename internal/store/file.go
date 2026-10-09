package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vigilante/internal/journal"
)

// fileStore is the single-node backend: the JSONL journal plus one lock file
// per lease next to it. Lock files make leases work across processes that
// share a filesystem (several CI jobs on one runner), not across machines.
type fileStore struct {
	path string
	j    *journal.Journal
	dir  string
	mu   sync.Mutex
}

func OpenFile(path string) (Store, error) {
	j, err := journal.Open(path)
	if err != nil {
		return nil, err
	}
	return &fileStore{path: path, j: j, dir: filepath.Dir(path)}, nil
}

func (f *fileStore) Describe() string { return "file:" + f.path }

// Append seals the entry onto the hash chain. Several processes (CI jobs)
// may share one journal file, so appends are serialised with a lock file and
// the chain head is re-read from the file's tail each time.
func (f *fileStore) Append(_ context.Context, e journal.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	unlock, err := f.lockAppend()
	if err != nil {
		return err
	}
	defer unlock()
	head, err := tailHash(f.path)
	if err != nil {
		return err
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Chain(head)
	return f.j.Append(e)
}

func (f *fileStore) Load(context.Context) (*journal.State, error) { return journal.Replay(f.path) }

func (f *fileStore) Scan(_ context.Context, fn func(pos int64, e journal.Entry) error) error {
	fh, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var line int64
	for sc.Scan() {
		line++
		var e journal.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("line %d: unreadable entry: %w", line, err)
		}
		if err := fn(line, e); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Prune rewrites the journal: the pruned prefix goes to archive, an anchor
// replaces it. The journal is rewritten under the append lock and replaced
// atomically; run it while no server is using this file.
func (f *fileStore) Prune(ctx context.Context, before time.Time, archive io.Writer) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	unlock, err := f.lockAppend()
	if err != nil {
		return 0, err
	}
	defer unlock()
	var all []journal.Entry
	if err := f.Scan(ctx, func(_ int64, e journal.Entry) error { all = append(all, e); return nil }); err != nil {
		return 0, err
	}
	cut := 0
	for cut < len(all) && all[cut].Time.Before(before) {
		cut++
	}
	if cut == 0 {
		return 0, nil
	}
	if err := writeArchive(archive, all[:cut]); err != nil {
		return 0, err
	}
	rest := append([]journal.Entry{anchorFor(all[:cut], before)}, all[cut:]...)
	tmp := f.path + ".prune.tmp"
	fh, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	if err := writeArchive(fh, rest); err != nil {
		fh.Close()
		return 0, err
	}
	if err := fh.Close(); err != nil {
		return 0, err
	}
	if err := f.j.Close(); err != nil { // Windows cannot replace an open file
		return 0, err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return 0, err
	}
	if f.j, err = journal.Open(f.path); err != nil {
		return 0, err
	}
	return cut, nil
}

// lockAppend takes an exclusive lock file for one append (stale after 10s).
func (f *fileStore) lockAppend() (func(), error) {
	path := f.path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fh.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 10*time.Second {
			os.Remove(path) // left by a crashed process
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("journal %s is locked by another process", f.path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tailHash returns the hash of the last entry in the file ("" if none).
func tailHash(path string) (string, error) {
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return "", err
	}
	// Entries are single lines well under 1 MiB; read the last chunk.
	size := st.Size()
	chunk := int64(1 << 20)
	if size < chunk {
		chunk = size
	}
	buf := make([]byte, chunk)
	if _, err := fh.ReadAt(buf, size-chunk); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Hash string `json:"hash"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) == nil {
			return e.Hash, nil // "" for a legacy (pre-chain) entry: the chain starts fresh
		}
	}
	return "", nil
}

// Fence is a no-op: a file store has a single writer by construction.
func (f *fileStore) Fence(string, string) {}

func (f *fileStore) Close() error { return f.j.Close() }

type leaseFile struct {
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

func (f *fileStore) leasePath(key string) string {
	return filepath.Join(f.dir, "vigilante-"+sanitize(key)+".lock")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, s)
}

func readLease(path string) (*leaseFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var l leaseFile
	if err := json.Unmarshal(b, &l); err != nil {
		// Unreadable (e.g. an old-format lock): treat as expired.
		return &leaseFile{}, nil
	}
	return &l, nil
}

func (f *fileStore) TryLease(_ context.Context, key, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := f.leasePath(key)
	now := time.Now()
	body, _ := json.Marshal(leaseFile{Owner: owner, Expires: now.Add(ttl)})
	// Fast path: create exclusively.
	if fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
		_, werr := fh.Write(body)
		cerr := fh.Close()
		return true, errors.Join(werr, cerr)
	} else if !errors.Is(err, os.ErrExist) {
		return false, err
	}
	cur, err := readLease(path)
	if err != nil {
		return false, err
	}
	if cur.Owner != owner && now.Before(cur.Expires) {
		return false, nil
	}
	// Ours (renew) or expired (take over): replace atomically.
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func (f *fileStore) ReleaseLease(_ context.Context, key, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := f.leasePath(key)
	cur, err := readLease(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Owner != owner {
		return nil
	}
	return os.Remove(path)
}

func (f *fileStore) LeaseHolder(_ context.Context, key string) (string, error) {
	cur, err := readLease(f.leasePath(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if time.Now().After(cur.Expires) {
		return "", nil
	}
	return cur.Owner, nil
}
