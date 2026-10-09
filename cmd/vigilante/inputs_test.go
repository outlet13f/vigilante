package main

import (
	"errors"
	"strings"
	"testing"

	"vigilante/internal/model"
)

type fakeJournal struct {
	live map[string]*model.Deployment
	good map[string]string
}

func (f fakeJournal) Live(id string) *model.Deployment { return f.live[id] }
func (f fakeJournal) LastGoodVersion(s string) (string, string, bool) {
	v, ok := f.good[s]
	return v, "dep-prev", ok
}

func noGit() (string, error) { return "", errors.New("no git") }

func envOf(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func TestResolveInputsFromCIAndJournal(t *testing.T) {
	c := &common{service: "order-api"}
	j := fakeJournal{good: map[string]string{"order-api": "v41"}}
	notes := resolveInputs(c, j, envOf(map[string]string{"CI_PIPELINE_ID": "77", "CI_COMMIT_SHORT_SHA": "abc12345"}), noGit)
	if c.id != "gl-77" || c.ver != "abc12345" || c.prev != "v41" {
		t.Fatalf("got id=%q ver=%q prev=%q", c.id, c.ver, c.prev)
	}
	if len(notes) != 3 || !strings.Contains(notes[2], "last good deployment dep-prev") {
		t.Fatalf("notes %v", notes)
	}
}

func TestResolveInputsKeepsExplicitFlags(t *testing.T) {
	c := &common{service: "order-api", id: "rel-1", ver: "v42", prev: "v40"}
	j := fakeJournal{good: map[string]string{"order-api": "v41"}}
	if notes := resolveInputs(c, j, envOf(map[string]string{"BUILD_TAG": "jenkins-x-1", "GIT_COMMIT": "ffff0000aaaa"}), noGit); len(notes) != 0 {
		t.Fatalf("explicit flags were overridden: %v", notes)
	}
	if c.id != "rel-1" || c.ver != "v42" || c.prev != "v40" {
		t.Fatalf("got %+v", c)
	}
}

func TestResolveInputsExistingDeploymentWins(t *testing.T) {
	c := &common{service: "order-api"}
	j := fakeJournal{live: map[string]*model.Deployment{"jenkins-order-9": {ID: "jenkins-order-9"}}, good: map[string]string{"order-api": "v41"}}
	resolveInputs(c, j, envOf(map[string]string{"BUILD_TAG": "jenkins-order-9", "GIT_COMMIT": "ffff0000aaaa"}), noGit)
	if c.id != "jenkins-order-9" || c.ver != "" || c.prev != "" {
		t.Fatalf("existing deployment must keep its own versions, got %+v", c)
	}
}

func TestRequireRollbackTarget(t *testing.T) {
	if err := requireRollbackTarget(&model.Deployment{Service: "s", Version: "v2"}); err == nil || !strings.Contains(err.Error(), "mark-good") {
		t.Fatalf("missing previous must point to mark-good, got %v", err)
	}
	withCheckpoint := &model.Deployment{Service: "s", Version: "v2", Checkpoints: map[string]map[string]string{"a": {"symlink.previous": "/opt/x"}}}
	if err := requireRollbackTarget(withCheckpoint); err != nil {
		t.Fatalf("a prepare checkpoint is enough: %v", err)
	}
	if err := requireRollbackTarget(&model.Deployment{Service: "s", PreviousVersion: "v1"}); err == nil {
		t.Fatal("missing version must be rejected")
	}
}
