// Package gpu holds static knowledge about NVIDIA data-center GPU models:
// memory sizes, the MIG profiles each model supports, and helpers to map the
// free-form model strings reported by DCGM and GPU Feature Discovery onto it.
package gpu

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MiBPerGB is the number of mebibytes in one decimal gigabyte. MIG profile
// names ("1g.10gb") are nominal; treating the figure as decimal gigabytes
// gives a lower bound on the memory a slice really exposes (a 1g.10gb slice
// reports roughly 9.7k MiB), which keeps fit checks on the safe side.
const MiBPerGB = 1e9 / (1 << 20)

// MIGProfile is one GPU-instance profile of a MIG-capable model.
type MIGProfile struct {
	// Name is the profile name as used by nvidia-smi and the device plugin,
	// for example "1g.10gb".
	Name string `json:"name"`
	// MemoryGB is the nominal memory from the profile name.
	MemoryGB int `json:"memoryGB"`
	// MemorySlices / MemoryTotal is the fraction of GPU memory.
	MemorySlices int `json:"memorySlices"`
	MemoryTotal  int `json:"memoryTotal"`
	// ComputeSlices / ComputeTotal is the fraction of streaming
	// multiprocessors (compute).
	ComputeSlices int `json:"computeSlices"`
	ComputeTotal  int `json:"computeTotal"`
	// MaxInstances is how many instances of this profile fit on one GPU.
	MaxInstances int `json:"maxInstances"`
}

// MemoryMiB is the conservative usable memory of the profile in MiB.
func (p MIGProfile) MemoryMiB() float64 { return float64(p.MemoryGB) * MiBPerGB }

// MemoryFraction is the share of the GPU's memory the profile takes.
func (p MIGProfile) MemoryFraction() float64 {
	return float64(p.MemorySlices) / float64(p.MemoryTotal)
}

// ComputeFraction is the share of the GPU's compute the profile takes.
func (p MIGProfile) ComputeFraction() float64 {
	return float64(p.ComputeSlices) / float64(p.ComputeTotal)
}

// Share is the fraction of a whole GPU that the profile occupies for cost
// purposes: whichever of memory or compute is the scarcer dimension.
func (p MIGProfile) Share() float64 {
	return math.Max(p.MemoryFraction(), p.ComputeFraction())
}

// IsFull reports whether the profile spans the entire GPU.
func (p MIGProfile) IsFull() bool {
	return p.MemorySlices == p.MemoryTotal && p.ComputeSlices == p.ComputeTotal
}

// Model describes one GPU model (and memory variant).
type Model struct {
	// Key is the stable identifier used in price tables and JSON output,
	// for example "a100-80gb".
	Key string `json:"key"`
	// Family groups memory variants of the same chip, for example "a100".
	Family string `json:"family"`
	// Short is the name used in the compact table, for example "A100".
	Short string `json:"short"`
	// Display is the unambiguous name, for example "A100 80GB".
	Display string `json:"display"`
	// MemoryGB is the nominal memory size.
	MemoryGB int `json:"memoryGB"`
	// MIG lists the supported GPU-instance profiles, smallest first. Empty
	// for models without MIG support.
	MIG []MIGProfile `json:"mig,omitempty"`
	// Throughput is a coarse relative-performance index (A100 = 1.0). It is
	// only used to decide whether a smaller model is a plausible target and
	// is always applied with a wide safety margin.
	Throughput float64 `json:"throughput"`
	// Suggest marks models that are sensible downsizing targets (current,
	// commonly available parts).
	Suggest bool `json:"suggest"`
}

// MemoryMiB is the nominal memory of the model in MiB (conservative, see
// MiBPerGB).
func (m *Model) MemoryMiB() float64 { return float64(m.MemoryGB) * MiBPerGB }

// SupportsMIG reports whether the model can be partitioned with MIG.
func (m *Model) SupportsMIG() bool { return len(m.MIG) > 0 }

// Profile returns the MIG profile with the given name.
func (m *Model) Profile(name string) (MIGProfile, bool) {
	name = NormalizeProfileName(name)
	for _, p := range m.MIG {
		if p.Name == name {
			return p, true
		}
	}
	return MIGProfile{}, false
}

// sevenSlice builds the profile set shared by every 7-compute-slice,
// 8-memory-slice MIG GPU (A100, H100, H200, B200 ...). Only the memory sizes
// in the profile names differ between models.
func sevenSlice(eighth, quarter, half, full int) []MIGProfile {
	return []MIGProfile{
		{Name: fmt.Sprintf("1g.%dgb", eighth), MemoryGB: eighth, MemorySlices: 1, MemoryTotal: 8, ComputeSlices: 1, ComputeTotal: 7, MaxInstances: 7},
		{Name: fmt.Sprintf("1g.%dgb", quarter), MemoryGB: quarter, MemorySlices: 2, MemoryTotal: 8, ComputeSlices: 1, ComputeTotal: 7, MaxInstances: 4},
		{Name: fmt.Sprintf("2g.%dgb", quarter), MemoryGB: quarter, MemorySlices: 2, MemoryTotal: 8, ComputeSlices: 2, ComputeTotal: 7, MaxInstances: 3},
		{Name: fmt.Sprintf("3g.%dgb", half), MemoryGB: half, MemorySlices: 4, MemoryTotal: 8, ComputeSlices: 3, ComputeTotal: 7, MaxInstances: 2},
		{Name: fmt.Sprintf("4g.%dgb", half), MemoryGB: half, MemorySlices: 4, MemoryTotal: 8, ComputeSlices: 4, ComputeTotal: 7, MaxInstances: 1},
		{Name: fmt.Sprintf("7g.%dgb", full), MemoryGB: full, MemorySlices: 8, MemoryTotal: 8, ComputeSlices: 7, ComputeTotal: 7, MaxInstances: 1},
	}
}

// models is the built-in catalog. MIG tables follow the "Supported MIG
// Profiles" chapter of the NVIDIA MIG User Guide. Variants that only add
// media extensions or graphics (+me, +gfx) are left out: they have the same
// memory and compute as their base profile.
var models = []*Model{
	{Key: "t4", Family: "t4", Short: "T4", Display: "T4", MemoryGB: 16, Throughput: 0.10, Suggest: true},
	{Key: "l4", Family: "l4", Short: "L4", Display: "L4", MemoryGB: 24, Throughput: 0.15, Suggest: true},
	{Key: "a10", Family: "a10", Short: "A10", Display: "A10", MemoryGB: 24, Throughput: 0.25, Suggest: true},
	{Key: "v100", Family: "v100", Short: "V100", Display: "V100 16GB", MemoryGB: 16, Throughput: 0.25},
	{Key: "v100-32gb", Family: "v100", Short: "V100", Display: "V100 32GB", MemoryGB: 32, Throughput: 0.25},
	{Key: "a30", Family: "a30", Short: "A30", Display: "A30", MemoryGB: 24, Throughput: 0.30, MIG: []MIGProfile{
		{Name: "1g.6gb", MemoryGB: 6, MemorySlices: 1, MemoryTotal: 4, ComputeSlices: 1, ComputeTotal: 4, MaxInstances: 4},
		{Name: "2g.12gb", MemoryGB: 12, MemorySlices: 2, MemoryTotal: 4, ComputeSlices: 2, ComputeTotal: 4, MaxInstances: 2},
		{Name: "4g.24gb", MemoryGB: 24, MemorySlices: 4, MemoryTotal: 4, ComputeSlices: 4, ComputeTotal: 4, MaxInstances: 1},
	}},
	{Key: "a40", Family: "a40", Short: "A40", Display: "A40", MemoryGB: 48, Throughput: 0.35},
	{Key: "l40", Family: "l40", Short: "L40", Display: "L40", MemoryGB: 48, Throughput: 0.50},
	{Key: "l40s", Family: "l40s", Short: "L40S", Display: "L40S", MemoryGB: 48, Throughput: 0.55, Suggest: true},
	{Key: "a100-40gb", Family: "a100", Short: "A100", Display: "A100 40GB", MemoryGB: 40, Throughput: 1.0, Suggest: true, MIG: sevenSlice(5, 10, 20, 40)},
	{Key: "a100-80gb", Family: "a100", Short: "A100", Display: "A100 80GB", MemoryGB: 80, Throughput: 1.0, Suggest: true, MIG: sevenSlice(10, 20, 40, 80)},
	{Key: "h100", Family: "h100", Short: "H100", Display: "H100 80GB", MemoryGB: 80, Throughput: 2.0, Suggest: true, MIG: sevenSlice(10, 20, 40, 80)},
	{Key: "h100-94gb", Family: "h100", Short: "H100", Display: "H100 94GB", MemoryGB: 94, Throughput: 2.0, MIG: sevenSlice(12, 24, 47, 94)},
	{Key: "h200", Family: "h200", Short: "H200", Display: "H200", MemoryGB: 141, Throughput: 2.2, MIG: sevenSlice(18, 35, 71, 141)},
	{Key: "b200", Family: "b200", Short: "B200", Display: "B200", MemoryGB: 180, Throughput: 4.0, MIG: sevenSlice(23, 45, 90, 180)},
}

// Models returns the catalog ordered by key.
func Models() []*Model {
	out := make([]*Model, len(models))
	copy(out, models)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ByKey returns the model with the given catalog key.
func ByKey(key string) (*Model, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, m := range models {
		if m.Key == key {
			return m, true
		}
	}
	return nil, false
}

// IsFamily reports whether name is a known model family such as "a100".
func IsFamily(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range models {
		if m.Family == name {
			return true
		}
	}
	return false
}

// familyTokens maps tokens found in product strings to a family. A10G is the
// AWS variant of the A10 and is treated as the same model.
var familyTokens = map[string]string{
	"T4": "t4", "L4": "l4", "A10": "a10", "A10G": "a10", "V100": "v100",
	"A30": "a30", "A40": "a40", "L40": "l40", "L40S": "l40s",
	"A100": "a100", "H100": "h100", "H200": "h200", "B200": "b200",
}

var (
	splitRe    = regexp.MustCompile(`[\s\-_/]+`)
	memTokenRe = regexp.MustCompile(`^(\d+)GB$`)
	migSuffix  = regexp.MustCompile(`(?i)[\s\-_]MIG[\s\-_](\d+g\.\d+gb)`)
)

// Resolve maps a product string such as "NVIDIA A100-SXM4-80GB", "Tesla T4"
// or the GFD label value "NVIDIA-H100-80GB-HBM3" to a catalog model.
//
// capacityMiB is the framebuffer size observed for the device (0 if
// unknown). It is used to pick the memory variant when the name alone is
// ambiguous. When neither settles it, the smallest variant is returned so
// that downstream fit checks err on the conservative side.
func Resolve(name string, capacityMiB float64) (*Model, bool) {
	// A MIG suffix ("...-MIG-1g.5gb") describes the slice, not the card.
	name = migSuffix.ReplaceAllString(name, "")
	family := ""
	memGB := 0
	for _, tok := range splitRe.Split(strings.ToUpper(name), -1) {
		if f, ok := familyTokens[tok]; ok && family == "" {
			family = f
			continue
		}
		if m := memTokenRe.FindStringSubmatch(tok); m != nil && memGB == 0 {
			memGB, _ = strconv.Atoi(m[1])
		}
	}
	if family == "" {
		return nil, false
	}
	var variants []*Model
	for _, m := range models {
		if m.Family == family {
			variants = append(variants, m)
		}
	}
	sort.Slice(variants, func(i, j int) bool { return variants[i].MemoryGB < variants[j].MemoryGB })
	if memGB > 0 {
		for _, m := range variants {
			if m.MemoryGB == memGB {
				return m, true
			}
		}
	}
	if capacityMiB > 0 && len(variants) > 1 {
		best, bestDiff := variants[0], math.MaxFloat64
		for _, m := range variants {
			// Observed capacity is in MiB; nominal sizes are closer to GiB.
			if d := math.Abs(capacityMiB - float64(m.MemoryGB)*1024); d < bestDiff {
				best, bestDiff = m, d
			}
		}
		return best, true
	}
	return variants[0], true
}

// MIGProfileFromProduct extracts the profile from a GFD product label of a
// node using the "single" MIG strategy ("A100-SXM4-40GB-MIG-1g.5gb").
func MIGProfileFromProduct(product string) string {
	if m := migSuffix.FindStringSubmatch(product); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// NormalizeProfileName lower-cases a MIG profile name and strips the
// optional "+me"/"+gfx" style suffixes and a leading "MIG " or "mig-".
func NormalizeProfileName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "mig-")
	name = strings.TrimPrefix(name, "mig ")
	if i := strings.IndexAny(name, "+"); i >= 0 {
		name = name[:i]
	}
	return name
}

// SuggestTargets returns the models that may be proposed as downsizing
// targets.
func SuggestTargets() []*Model {
	var out []*Model
	for _, m := range models {
		if m.Suggest {
			out = append(out, m)
		}
	}
	return out
}
