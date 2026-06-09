package tui

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestKnownHostKeyAlgorithmsMatchesStoredEntry(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{"10.137.200.71:22"}, sshPub)
	if err := os.WriteFile(khPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(khPath)
	if err != nil {
		t.Fatal(err)
	}

	algos := knownHostKeyAlgorithms(cb, "10.137.200.71:22")
	if len(algos) != 1 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algos = %v, want [%s]", algos, ssh.KeyAlgoED25519)
	}

	// Unknown host -> no constraint, so TOFU still works.
	if got := knownHostKeyAlgorithms(cb, "192.168.1.166:22"); len(got) != 0 {
		t.Fatalf("unknown host algos = %v, want empty", got)
	}
}
