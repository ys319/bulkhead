// Package sshkey handles the ssh keys bh deals with: the machine key it
// generates and the public keys users register.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Fingerprint is the OpenSSH SHA256 fingerprint of a public key line (what
// `ssh-keygen -lf` prints): "SHA256:" and 43 base64 characters.
func Fingerprint(publicKeyLine string) (string, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeyLine))
	if err != nil {
		return "", fmt.Errorf("not an ssh public key line: %w", err)
	}
	return ssh.FingerprintSHA256(key), nil
}

// EnsureMachineKey returns the public key line of the key pair at
// privatePath, generating the pair when there is none. There is one key per
// machine, not per environment; nothing here ever replaces an existing key.
func EnsureMachineKey(privatePath, comment string) (string, error) {
	publicPath := privatePath + ".pub"
	existing, err := os.ReadFile(publicPath)
	if err == nil {
		return strings.TrimSpace(string(existing)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	// Only the private half is there (a partial restore, a deleted .pub): the
	// public half follows from it. Generating a new pair would lock the user
	// out of every environment that trusts the old one.
	if pemBytes, err := os.ReadFile(privatePath); err == nil {
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return "", fmt.Errorf("reading %s to restore %s: %w", privatePath, publicPath, err)
		}
		line := publicLine(signer.PublicKey(), comment)
		return line, os.WriteFile(publicPath, []byte(line+"\n"), 0o644)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return "", err
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(privatePath), 0o755); err != nil {
		return "", err
	}
	// O_EXCL: never write over a key that appeared in the meantime.
	file, err := os.OpenFile(privatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(pem.EncodeToMemory(block)); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	line := publicLine(sshPublic, comment)
	return line, os.WriteFile(publicPath, []byte(line+"\n"), 0o644)
}

func publicLine(key ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if comment != "" {
		line += " " + comment
	}
	return line
}
