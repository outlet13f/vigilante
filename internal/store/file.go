package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func (f *fileStore) Append(_ context.Context, e journal.Entry) error { return f.j.Append(e) }

func (f *fileStore) Load(context.Context) (*journal.State, error) { return journal.Replay(f.path) }

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
