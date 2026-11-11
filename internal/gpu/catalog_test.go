package gpu

import (
	"math"
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		capacity float64
		want     string
	}{
		{"NVIDIA A100-SXM4-80GB", 0, "a100-80gb"},
		{"NVIDIA A100-SXM4-40GB", 0, "a100-40gb"},
		{"NVIDIA A100 80GB PCIe", 0, "a100-80gb"},
		{"NVIDIA A100-PCIE-40GB", 0, "a100-40gb"},
		{"A100", 0, "a100-40gb"},     // ambiguous: smallest variant
		{"A100", 81000, "a100-80gb"}, // capacity settles it
		{"A100", 40500, "a100-40gb"},
		{"NVIDIA H100 80GB HBM3", 0, "h100"},
		{"NVIDIA H100 PCIe", 0, "h100"},
		{"NVIDIA H100 NVL", 95800, "h100-94gb"},
		{"NVIDIA-H100-80GB-HBM3", 0, "h100"},
		{"NVIDIA H200", 0, "h200"},
		{"Tesla T4", 0, "t4"},
		{"Tesla-T4-SHARED", 0, "t4"},
		{"NVIDIA L4", 0, "l4"},
		{"NVIDIA L40S", 0, "l40s"},
		{"NVIDIA L40", 0, "l40"},
		{"NVIDIA A10G", 0, "a10"},
		{"NVIDIA A30", 0, "a30"},
		{"Tesla V100-SXM2-16GB", 0, "v100"},
		{"Tesla V100-SXM2-32GB", 0, "v100-32gb"},
		{"A100-SXM4-40GB-MIG-1g.5gb", 0, "a100-40gb"},
		{"NVIDIA B200", 0, "b200"},
	}
	for _, tt := range tests {
		got, ok := Resolve(tt.name, tt.capacity)
		if !ok {
			t.Errorf("Resolve(%q) not found", tt.name)
			continue
		}
		if got.Key != tt.want {
			t.Errorf("Resolve(%q, %v) = %s, want %s", tt.name, tt.capacity, got.Key, tt.want)
		}
	}
	for _, name := range []string{"", "Quadro K2200", "GeForce RTX 4090"} {
		if m, ok := Resolve(name, 0); ok {
			t.Errorf("Resolve(%q) = %s, want not found", name, m.Key)
		}
	}
}

// TestMIGTable pins the catalog to the NVIDIA MIG User Guide.
func TestMIGTable(t *testing.T) {
	type row struct {
		name         string
		memFrac      float64
		computeFrac  float64
		maxInstances int
	}
	want := map[string][]row{
		"a30": {
			{"1g.6gb", 1.0 / 4, 1.0 / 4, 4},
			{"2g.12gb", 2.0 / 4, 2.0 / 4, 2},
			{"4g.24gb", 1, 1, 1},
		},
		"a100-40gb": {
			{"1g.5gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.10gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.10gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.20gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.20gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.40gb", 1, 1, 1},
		},
		"a100-80gb": {
			{"1g.10gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.20gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.20gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.40gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.40gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.80gb", 1, 1, 1},
		},
		"h100": {
			{"1g.10gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.20gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.20gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.40gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.40gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.80gb", 1, 1, 1},
		},
		"h100-94gb": {
			{"1g.12gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.24gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.24gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.47gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.47gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.94gb", 1, 1, 1},
		},
		"h200": {
			{"1g.18gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.35gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.35gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.71gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.71gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.141gb", 1, 1, 1},
		},
		"b200": {
			{"1g.23gb", 1.0 / 8, 1.0 / 7, 7},
			{"1g.45gb", 2.0 / 8, 1.0 / 7, 4},
			{"2g.45gb", 2.0 / 8, 2.0 / 7, 3},
			{"3g.90gb", 4.0 / 8, 3.0 / 7, 2},
			{"4g.90gb", 4.0 / 8, 4.0 / 7, 1},
			{"7g.180gb", 1, 1, 1},
		},
	}
	for key, rows := range want {
		m, ok := ByKey(key)
		if !ok {
			t.Fatalf("model %s missing", key)
		}
		if len(m.MIG) != len(rows) {
			t.Fatalf("%s: %d profiles, want %d", key, len(m.MIG), len(rows))
		}
		for i, r := range rows {
			p := m.MIG[i]
			if p.Name != r.name || p.MaxInstances != r.maxInstances ||
				math.Abs(p.MemoryFraction()-r.memFrac) > 1e-9 ||
				math.Abs(p.ComputeFraction()-r.computeFrac) > 1e-9 {
				t.Errorf("%s[%d] = %+v, want %+v", key, i, p, r)
			}
			if got, ok := m.Profile(r.name); !ok || got.Name != r.name {
				t.Errorf("%s.Profile(%q) lookup failed", key, r.name)
			}
		}
		if !m.MIG[len(m.MIG)-1].IsFull() {
			t.Errorf("%s: last profile should span the GPU", key)
		}
	}
	for _, m := range Models() {
		_, listed := want[m.Key]
		if m.SupportsMIG() != listed {
			t.Errorf("%s: SupportsMIG = %v, table coverage = %v", m.Key, m.SupportsMIG(), listed)
		}
	}
}

func TestProfileInvariants(t *testing.T) {
	for _, m := range Models() {
		prevShare := 0.0
		for _, p := range m.MIG {
			if p.MemoryMiB() > m.MemoryMiB()+1e-6 {
				t.Errorf("%s %s: slice larger than card", m.Key, p.Name)
			}
			if p.Share() < prevShare {
				t.Errorf("%s: profiles not ordered by share at %s", m.Key, p.Name)
			}
			prevShare = p.Share()
		}
	}
}

func TestShare(t *testing.T) {
	m, _ := ByKey("a100-80gb")
	want := map[string]float64{"1g.10gb": 1.0 / 7, "1g.20gb": 1.0 / 4, "2g.20gb": 2.0 / 7, "3g.40gb": 1.0 / 2, "4g.40gb": 4.0 / 7, "7g.80gb": 1}
	for name, share := range want {
		p, _ := m.Profile(name)
		if math.Abs(p.Share()-share) > 1e-9 {
			t.Errorf("%s share = %v, want %v", name, p.Share(), share)
		}
	}
}

func TestNormalizeProfileName(t *testing.T) {
	for in, want := range map[string]string{
		"1g.10gb": "1g.10gb", "MIG 1g.10gb": "1g.10gb", "mig-3g.40gb": "3g.40gb", "1g.10gb+me": "1g.10gb", " 2G.20GB ": "2g.20gb",
	} {
		if got := NormalizeProfileName(in); got != want {
			t.Errorf("NormalizeProfileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MIGProfileFromProduct("A100-SXM4-40GB-MIG-1g.5gb-SHARED"); got != "1g.5gb" {
		t.Errorf("MIGProfileFromProduct = %q", got)
	}
	if got := MIGProfileFromProduct("Tesla-T4"); got != "" {
		t.Errorf("MIGProfileFromProduct = %q, want empty", got)
	}
}
