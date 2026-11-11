package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
)

func model(t *testing.T, key string) *gpu.Model {
	t.Helper()
	m, ok := gpu.ByKey(key)
	if !ok {
		t.Fatalf("no model %s", key)
	}
	return m
}

func TestDefaultsCoverCatalog(t *testing.T) {
	tbl := Defaults()
	for _, m := range gpu.Models() {
		if p, ok := tbl.Lookup(m); !ok || p <= 0 {
			t.Errorf("no default price for %s", m.Key)
		}
		if tbl.Overridden(m) {
			t.Errorf("%s reported as overridden", m.Key)
		}
	}
	if _, ok := tbl.Lookup(nil); ok {
		t.Error("Lookup(nil) should fail")
	}
}

func TestOverrides(t *testing.T) {
	tbl := Defaults()
	if err := tbl.ApplyOverrides([]string{"a100=3.67", "H100-94GB = $12.5"}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a100-40gb", "a100-80gb"} {
		if p, _ := tbl.Lookup(model(t, key)); p != 3.67 {
			t.Errorf("%s = %v, want family override 3.67", key, p)
		}
		if !tbl.Overridden(model(t, key)) {
			t.Errorf("%s should be overridden", key)
		}
	}
	if p, _ := tbl.Lookup(model(t, "h100-94gb")); p != 12.5 {
		t.Errorf("h100-94gb = %v", p)
	}
	if p, _ := tbl.Lookup(model(t, "h100")); p != 9.80 {
		t.Errorf("h100 changed to %v", p)
	}
	for _, bad := range []string{"a100", "a100=abc", "rtx9999=1", "t4=-1"} {
		if err := Defaults().ApplyOverrides([]string{bad}); err == nil {
			t.Errorf("ApplyOverrides(%q) should fail", bad)
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(good, []byte("prices:\n  a100-80gb: 2.1\n  t4: 0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl := Defaults()
	if err := tbl.LoadFile(good); err != nil {
		t.Fatal(err)
	}
	if p, _ := tbl.Lookup(model(t, "a100-80gb")); p != 2.1 {
		t.Errorf("a100-80gb = %v", p)
	}
	if p, _ := tbl.Lookup(model(t, "a100-40gb")); p != 3.67 {
		t.Errorf("a100-40gb = %v, should keep default", p)
	}

	cases := map[string]string{
		"empty.yaml":   "prices: {}\n",
		"unknown.yaml": "prices:\n  warp9: 1\n",
		"field.yaml":   "price:\n  t4: 1\n",
		"syntax.yaml":  "prices: [\n",
	}
	for name, content := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Defaults().LoadFile(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := Defaults().LoadFile(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "reading price file") {
		t.Errorf("missing file: %v", err)
	}
}
