// Package pricing provides the hourly GPU price table used to turn
// recommendations into an estimated monthly waste figure.
package pricing

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/gpu"
)

// HoursPerMonth is the average number of hours in a month (8760 / 12).
const HoursPerMonth = 730.0

// defaults are ROUGH on-demand list-price estimates in USD per GPU-hour.
// They are ballpark figures for single-GPU instances on large public clouds
// and are only meant to make the waste column meaningful out of the box.
// Real prices vary widely by provider, region, commitment and over time:
// pass --price or --price-file to use your own.
var defaults = map[string]float64{
	"t4":        0.53,
	"l4":        0.75,
	"a10":       1.01,
	"v100":      3.06,
	"a30":       1.10,
	"a40":       1.30,
	"l40":       1.60,
	"l40s":      1.86,
	"a100-40gb": 3.67,
	"a100-80gb": 5.07,
	"h100":      9.80,
	"h200":      10.60,
	"b200":      14.00,
}

// Table maps a model key ("a100-80gb") or family ("a100") to USD per
// GPU-hour.
type Table struct {
	prices map[string]float64
	// overridden records the keys set by the user, for reporting.
	overridden map[string]bool
}

// Defaults returns a copy of the built-in table.
func Defaults() *Table {
	t := &Table{prices: map[string]float64{}, overridden: map[string]bool{}}
	for k, v := range defaults {
		t.prices[k] = v
	}
	return t
}

// File is the schema of a --price-file document.
type File struct {
	// Prices maps a model key or family to USD per GPU-hour.
	Prices map[string]float64 `json:"prices"`
}

// LoadFile merges the prices from a YAML file into the table.
func (t *Table) LoadFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // path is an explicit user-supplied flag
	if err != nil {
		return fmt.Errorf("reading price file: %w", err)
	}
	var f File
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return fmt.Errorf("parsing price file %s: %w", path, err)
	}
	if len(f.Prices) == 0 {
		return fmt.Errorf("price file %s: no entries under \"prices\"", path)
	}
	keys := make([]string, 0, len(f.Prices))
	for k := range f.Prices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := t.Set(k, f.Prices[k]); err != nil {
			return fmt.Errorf("price file %s: %w", path, err)
		}
	}
	return nil
}

// ApplyOverrides applies "model=price" pairs as given to --price.
func (t *Table) ApplyOverrides(pairs []string) error {
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("invalid --price %q: want model=usd-per-hour, e.g. a100=3.67", pair)
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "$")), 64)
		if err != nil {
			return fmt.Errorf("invalid --price %q: %q is not a number", pair, value)
		}
		if err := t.Set(name, price); err != nil {
			return err
		}
	}
	return nil
}

// Set sets the price of a model key or of every variant in a family.
func (t *Table) Set(name string, price float64) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if price < 0 {
		return fmt.Errorf("price for %q must not be negative", name)
	}
	if _, ok := gpu.ByKey(name); ok {
		t.prices[name] = price
		t.overridden[name] = true
		return nil
	}
	if gpu.IsFamily(name) {
		for _, m := range gpu.Models() {
			if m.Family == name {
				t.prices[m.Key] = price
				t.overridden[m.Key] = true
			}
		}
		return nil
	}
	return fmt.Errorf("unknown GPU model %q (known: %s)", name, strings.Join(KnownNames(), ", "))
}

// Lookup returns the hourly price for a model.
func (t *Table) Lookup(m *gpu.Model) (float64, bool) {
	if m == nil {
		return 0, false
	}
	if p, ok := t.prices[m.Key]; ok {
		return p, true
	}
	// Memory variants without their own entry inherit the family price.
	if p, ok := t.prices[m.Family]; ok {
		return p, true
	}
	for _, other := range gpu.Models() {
		if other.Family == m.Family {
			if p, ok := t.prices[other.Key]; ok {
				return p, true
			}
		}
	}
	return 0, false
}

// Overridden reports whether the user supplied the price for a model.
func (t *Table) Overridden(m *gpu.Model) bool {
	return m != nil && t.overridden[m.Key]
}

// KnownNames lists every accepted model key and family.
func KnownNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range gpu.Models() {
		for _, n := range []string{m.Key, m.Family} {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}
