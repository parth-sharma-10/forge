package cgroup

import (
	"testing"
)

func TestLimitsFiles(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		want   []file
	}{
		{"unlimited writes nothing", Limits{}, nil},
		{
			"memory pins swap to zero too",
			Limits{MemoryBytes: 64 << 20},
			[]file{{"memory.max", "67108864"}, {"memory.swap.max", "0"}},
		},
		{"half a core", Limits{CPUCores: 0.5}, []file{{"cpu.max", "50000 100000"}}},
		{"two cores", Limits{CPUCores: 2}, []file{{"cpu.max", "200000 100000"}}},
		{"pids", Limits{MaxPIDs: 32}, []file{{"pids.max", "32"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.limits.files()
			if len(got) != len(tt.want) {
				t.Fatalf("files() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("files()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLimitsValidate(t *testing.T) {
	tests := []struct {
		name    string
		limits  Limits
		wantErr bool
	}{
		{"zero is valid", Limits{}, false},
		{"ordinary", Limits{MemoryBytes: 1 << 20, CPUCores: 1.5, MaxPIDs: 10}, false},
		{"negative memory", Limits{MemoryBytes: -1}, true},
		{"negative cpu", Limits{CPUCores: -0.5}, true},
		{"negative pids", Limits{MaxPIDs: -1}, true},
		// 0.001 cores is 100us of a 100000us period, under the kernel's floor
		// of 1000us. Rounding it up would grant ten times what was asked for.
		{"cpu below the kernel's minimum quota", Limits{CPUCores: 0.001}, true},
		{"smallest accepted cpu", Limits{CPUCores: 0.01}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.limits.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}
