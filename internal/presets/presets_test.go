package presets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsLoadAndResolve(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"java-web", "container-api", "static-web", "worker"} {
		p, err := r.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Version != 1 || p.Source != "builtin" || p.Description == "" {
			t.Fatalf("%s: %+v", name, p)
		}
		if _, err := p.Preview(nil); err != nil {
			t.Fatalf("%s preview: %v", name, err)
		}
	}
}

func TestLatestAndPinnedVersions(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"1", "2"} {
		os.WriteFile(filepath.Join(dir, "x"+v+".yaml"), []byte("name: corp/x\nversion: "+v+"\nparams: [{name: n, default: "+v+"}]\n---\nn: [[ .n ]]\n"), 0o644)
	}
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Get("corp/x"); p.Version != 2 {
		t.Fatalf("latest = %d", p.Version)
	}
	p, _ := r.Get("corp/x@1")
	if out, _ := p.Render(nil); strings.TrimSpace(out) != "n: 1" {
		t.Fatalf("pinned render %q", out)
	}
}

func TestDuplicateAndMalformedPresets(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dup.yaml"), []byte("name: java-web\nversion: 1\n---\nx: 1\n"), 0o644)
	if _, err := NewRegistry(dir); err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Fatalf("duplicate: %v", err)
	}
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "bad.yaml"), []byte("name: x\nversion: 1\n"), 0o644)
	if _, err := NewRegistry(dir2); err == nil || !strings.Contains(err.Error(), "missing '---'") {
		t.Fatalf("malformed: %v", err)
	}
	if _, err := NewRegistry(filepath.Join(dir2, "missing")); err == nil {
		t.Fatal("a missing preset dir must be an error")
	}
}
