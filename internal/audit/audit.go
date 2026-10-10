// Package audit verifies the tamper-evident journal and ships audit records
// to a SIEM.
//
// Every journal entry carries Prev and Hash (see journal.Entry.Chain), so
// changing or deleting any stored entry breaks the chain at that point;
// Verify walks the store (or an archive file) and reports the first break.
// The chain alone only proves consistency: whoever can write to the store
// can also recompute it. With audit.chain_key_ref every entry also carries
// an HMAC of its hash, which only holders of the key can produce.
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/store"
)

// Report is the result of a verification.
type Report struct {
	Entries int    `json:"entries"`
	Checked int    `json:"checked"` // chained entries whose hash matched
	Legacy  int    `json:"legacy"`  // entries written before the chain existed
	Head    string `json:"head"`    // hash of the last entry
	OK      bool   `json:"ok"`
	Broken  string `json:"broken,omitempty"`

	// With a chain key (audit.chain_key_ref): MACs checked, chained entries
	// written before the key was configured, and the position from which
	// the key protects the chain (0 = no entry carries a MAC).
	KeyChecked bool  `json:"key_checked"`
	Keyed      int   `json:"keyed,omitempty"`
	Unkeyed    int   `json:"unkeyed,omitempty"`
	KeyedFrom  int64 `json:"keyed_from,omitempty"`
}

// Verify checks the whole chain in a store; with key, also every entry's
// MAC (see journal.Verifier).
func Verify(ctx context.Context, st store.Store, key []byte) (Report, error) {
	v := journal.Verifier{Key: key}
	r := Report{}
	err := st.Scan(ctx, func(pos int64, e journal.Entry) error {
		r.Entries++
		return v.Next(pos, e)
	})
	return finish(r, &v, err)
}

// VerifyFile checks a JSONL archive written by Prune or Export.
func VerifyFile(path string, key []byte) (Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer f.Close()
	return VerifyReader(f, key)
}

func VerifyReader(rd io.Reader, key []byte) (Report, error) {
	v := journal.Verifier{Key: key}
	r := Report{}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var line int64
	var err error
	for sc.Scan() {
		line++
		var e journal.Entry
		if uerr := json.Unmarshal(sc.Bytes(), &e); uerr != nil {
			err = &journal.ErrChainBroken{Pos: line, Reason: "unreadable entry"}
			break
		}
		r.Entries++
		if err = v.Next(line, e); err != nil {
			break
		}
	}
	if err == nil {
		err = sc.Err()
	}
	return finish(r, &v, err)
}

func finish(r Report, v *journal.Verifier, err error) (Report, error) {
	r.Checked, r.Legacy, r.Head = v.Checked, v.Legacy, v.Head()
	r.KeyChecked, r.Keyed, r.Unkeyed, r.KeyedFrom = len(v.Key) > 0, v.Keyed, v.Unkeyed, v.KeyedFrom
	var broken *journal.ErrChainBroken
	switch {
	case errors.As(err, &broken):
		r.Broken = broken.Error()
		return r, nil
	case err != nil:
		return r, err
	}
	r.OK = true
	return r, nil
}

// Filter selects entries for export and queries.
type Filter struct {
	Since, Until time.Time       // zero = unbounded; Until is exclusive
	Kinds        map[string]bool // empty = audit-relevant kinds (everything but deployment snapshots and steps)
	Actor        string
	Service      string
	Action       string
	Limit        int
}

// Match reports whether an entry passes the filter.
func (f Filter) Match(e journal.Entry) bool {
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !e.Time.Before(f.Until) {
		return false
	}
	if len(f.Kinds) > 0 {
		if !f.Kinds[e.Kind] {
			return false
		}
	} else if journal.Bookkeeping(e.Kind) {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if f.Service != "" {
		svc := e.Service
		if svc == "" && e.Deployment != nil {
			svc = e.Deployment.Service
		}
		if svc != f.Service {
			return false
		}
	}
	return true
}

// Query returns matching entries (oldest first), at most f.Limit if set.
func Query(ctx context.Context, st store.Store, f Filter) ([]journal.Entry, error) {
	var out []journal.Entry
	errLimit := errors.New("limit")
	err := st.Scan(ctx, func(_ int64, e journal.Entry) error {
		if f.Match(e) {
			out = append(out, e)
			if f.Limit > 0 && len(out) >= f.Limit {
				return errLimit
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errLimit) {
		return nil, err
	}
	return out, nil
}

// Export writes every stored entry (all kinds, chain intact) to w as JSONL.
func Export(ctx context.Context, st store.Store, w io.Writer) (int, error) {
	enc := json.NewEncoder(w)
	n := 0
	err := st.Scan(ctx, func(_ int64, e journal.Entry) error {
		n++
		return enc.Encode(e)
	})
	return n, err
}
