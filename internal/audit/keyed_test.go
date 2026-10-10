package audit

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vigilante/internal/journal"
	"vigilante/internal/store"
)

var (
	chainKey = []byte("0123456789abcdef0123456789abcdef")
	otherKey = []byte("fedcba9876543210fedcba9876543210")
)

func TestKeyedChainDetectsRecomputedTampering(t *testing.T) {
	ctx := context.Background()
	for name, open := range backends(t) {
		for _, how := range []string{"keep-mac", "strip-mac"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				b := open(t)
				b.st.SetChainKey(chainKey)
				seed(t, b.st, 5, time.Now())
				pos := positions(t, b.st)
				r, err := Verify(ctx, b.st, chainKey)
				if err != nil || !r.OK || !r.KeyChecked || r.Keyed != 5 || r.Unkeyed != 0 || r.KeyedFrom != pos[0] {
					t.Fatalf("intact keyed chain: %+v %v", r, err)
				}
				// Rewrite entry 3 and recompute every hash after it; the
				// attacker cannot compute MACs, so it keeps or drops them.
				b.recompute(t, func(i int, e *journal.Entry) {
					if i == 2 {
						e.Actor = "user:mallory"
					}
					if how == "strip-mac" && i >= 2 {
						e.MAC = ""
					}
				})
				if r, err := Verify(ctx, b.st, nil); err != nil || !r.OK {
					t.Fatalf("without the key a recomputed chain verifies (the gap the key closes): %+v %v", r, err)
				}
				r, err = Verify(ctx, b.st, chainKey)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"keep-mac": "MAC does not match", "strip-mac": "has no MAC"}[how]
				if r.OK || !strings.Contains(r.Broken, want) || !strings.Contains(r.Broken, fmt.Sprintf("entry %d:", pos[2])) {
					t.Fatalf("recomputed chain (%s) not detected at entry %d: %+v", how, pos[2], r)
				}
			})
		}
	}
}

func TestKeyedChainWrongKey(t *testing.T) {
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			b.st.SetChainKey(chainKey)
			seed(t, b.st, 3, time.Now())
			r, err := Verify(context.Background(), b.st, otherKey)
			if err != nil || r.OK || !strings.Contains(r.Broken, "wrong chain key") {
				t.Fatalf("wrong key: %+v %v", r, err)
			}
		})
	}
}

func TestKeyedChainLegacyEntries(t *testing.T) {
	ctx := context.Background()
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			seed(t, b.st, 3, time.Now()) // written before the key was configured
			b.st.SetChainKey(chainKey)
			seed(t, b.st, 2, time.Now())
			pos := positions(t, b.st)
			r, err := Verify(ctx, b.st, chainKey)
			if err != nil || !r.OK || r.Checked != 5 || r.Unkeyed != 3 || r.Keyed != 2 || r.KeyedFrom != pos[3] {
				t.Fatalf("unkeyed prefix, keyed suffix: %+v %v", r, err)
			}
			if r, err := Verify(ctx, b.st, nil); err != nil || !r.OK || r.KeyChecked || r.Keyed != 0 {
				t.Fatalf("without a key MACs are ignored: %+v %v", r, err)
			}

			// An unkeyed entry after the key took effect: written by a
			// node without the key, or inserted by hand.
			b.st.SetChainKey(nil)
			seed(t, b.st, 1, time.Now())
			r, err = Verify(ctx, b.st, chainKey)
			if err != nil || r.OK || !strings.Contains(r.Broken, "has no MAC") || !strings.Contains(r.Broken, fmt.Sprintf("from entry %d on", pos[3])) {
				t.Fatalf("unkeyed entry after the keyed start: %+v %v", r, err)
			}
		})
	}
}

func TestKeyedChainCoversUnkeyedPrefix(t *testing.T) {
	ctx := context.Background()
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			seed(t, b.st, 3, time.Now())
			b.st.SetChainKey(chainKey)
			seed(t, b.st, 2, time.Now())
			pos := positions(t, b.st)
			// Rewriting a pre-key entry changes the hash every MAC'd entry
			// links to, so the first keyed entry no longer matches.
			b.recompute(t, func(i int, e *journal.Entry) {
				if i == 0 {
					e.Actor = "user:mallory"
				}
			})
			r, err := Verify(ctx, b.st, chainKey)
			if err != nil || r.OK || !strings.Contains(r.Broken, "MAC does not match") || !strings.Contains(r.Broken, fmt.Sprintf("entry %d:", pos[3])) {
				t.Fatalf("rewritten unkeyed prefix: %+v %v", r, err)
			}

			// Stripping every MAC makes the journal look as if no key was
			// ever used: verification passes but reports nothing keyed,
			// which the CLI flags and keyed_from lets operators compare.
			b.recompute(t, func(_ int, e *journal.Entry) { e.MAC = "" })
			r, err = Verify(ctx, b.st, chainKey)
			if err != nil || !r.OK || r.Keyed != 0 || r.KeyedFrom != 0 || r.Unkeyed != 5 {
				t.Fatalf("all MACs stripped: %+v %v", r, err)
			}
		})
	}
}

func TestResolveChainKey(t *testing.T) {
	ctx := context.Background()
	if k, err := store.ResolveChainKey(ctx, ""); k != nil || err != nil {
		t.Fatalf("no ref: %v %v", k, err)
	}
	t.Setenv("VIGILANTE_TEST_CHAIN_KEY", "too-short")
	if _, err := store.ResolveChainKey(ctx, "env:VIGILANTE_TEST_CHAIN_KEY"); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("short key accepted: %v", err)
	}
	t.Setenv("VIGILANTE_TEST_CHAIN_KEY_OK", string(chainKey)) // another name: resolved values are cached
	if k, err := store.ResolveChainKey(ctx, "env:VIGILANTE_TEST_CHAIN_KEY_OK"); err != nil || !bytes.Equal(k, chainKey) {
		t.Fatalf("key: %q %v", k, err)
	}
}

func TestKeyedChainAcrossPrune(t *testing.T) {
	ctx := context.Background()
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			b.st.SetChainKey(chainKey)
			seed(t, b.st, 3, time.Now().Add(-400*24*time.Hour))
			seed(t, b.st, 2, time.Now())
			var archive bytes.Buffer
			if n, err := b.st.Prune(ctx, time.Now().Add(-365*24*time.Hour), &archive); err != nil || n != 3 {
				t.Fatalf("pruned %d: %v", n, err)
			}
			if r, err := VerifyReader(&archive, chainKey); err != nil || !r.OK || r.Keyed != 3 {
				t.Fatalf("archive with the key: %+v %v", r, err)
			}
			pos := positions(t, b.st)
			r, err := Verify(ctx, b.st, chainKey)
			if err != nil || !r.OK || r.Checked != 2 || r.Keyed != 3 || r.KeyedFrom != pos[0] {
				t.Fatalf("anchor carries the last pruned MAC: %+v %v", r, err)
			}
			if r, _ := Verify(ctx, b.st, otherKey); r.OK {
				t.Fatalf("anchor MAC accepted with the wrong key: %+v", r)
			}
		})
	}
}
