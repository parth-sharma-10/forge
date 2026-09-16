package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpecValidate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		spec    Spec
		wantErr bool
	}{
		{"ok", Spec{Rootfs: dir, Args: []string{"/bin/sh"}}, false},
		{"no command", Spec{Rootfs: dir}, true},
		{"relative rootfs", Spec{Rootfs: "rootfs", Args: []string{"/bin/sh"}}, true},
		{"missing rootfs", Spec{Rootfs: filepath.Join(dir, "absent"), Args: []string{"/bin/sh"}}, true},
		{"rootfs is a file", Spec{Rootfs: file, Args: []string{"/bin/sh"}}, true},
		// A separator in the path would be read as the end of an overlay
		// mount option, and the mount would fail with a bare EINVAL.
		{"comma in rootfs", Spec{Rootfs: separator(t, dir, "a,b"), Args: []string{"/bin/sh"}}, true},
		{"colon in rootfs", Spec{Rootfs: separator(t, dir, "a:b"), Args: []string{"/bin/sh"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestSpecValidateDefaultsHostname(t *testing.T) {
	spec := Spec{Rootfs: t.TempDir(), Args: []string{"/bin/sh"}}
	if err := spec.validate(); err != nil {
		t.Fatal(err)
	}
	if spec.Hostname != defaultHostname {
		t.Fatalf("hostname = %q, want %q", spec.Hostname, defaultHostname)
	}
}

// separator makes a real directory whose name holds an overlay option
// separator, so the test reaches the check rather than failing on Stat.
func separator(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
