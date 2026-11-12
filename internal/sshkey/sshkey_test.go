package sshkey

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	// ssh-keygen -lf on this line prints the fingerprint below.
	const line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIESD9O48y6Q1airXig+Rz77VFzZ5LlDMfbOtr2U9FH+N someone@example"
	const want = "SHA256:UPz/tYMFZj8joscMirdI1bpNKZGbx8PZ3zVMEvb9kZ8"

	got, err := Fingerprint(line)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("fingerprint = %s, want %s", got, want)
	}
	if _, err := Fingerprint("not a key"); err == nil {
		t.Error("a line that is not a key was accepted")
	}
}

func TestEnsureMachineKeyGeneratesOnceAndKeepsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "id_ed25519")

	first, err := EnsureMachineKey(path, "bulkhead")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^ssh-ed25519 \S+ bulkhead$`).MatchString(first) {
		t.Errorf("public key line = %q", first)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("private key: %v, mode %v, want 0600", err, info.Mode().Perm())
	}
	private, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(private), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Errorf("private key is not in OpenSSH format: %.40q", private)
	}

	second, err := EnsureMachineKey(path, "other")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Error("a second call produced a different key")
	}
}

func TestEnsureMachineKeyRestoresAMissingPublicHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	first, err := EnsureMachineKey(path, "bulkhead")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".pub"); err != nil {
		t.Fatal(err)
	}

	restored, err := EnsureMachineKey(path, "bulkhead")
	if err != nil {
		t.Fatal(err)
	}
	if restored != first {
		t.Errorf("restored %q, want the original %q", restored, first)
	}
}
