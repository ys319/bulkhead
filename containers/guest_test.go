package containers_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bh-guest env turns the file the host wrote — one NAME=value line each —
// into exports a shell evaluates. Values are text of the user's: nothing in
// them may be run, expanded or cut short.
func TestGuestExportsValuesAsTheyAre(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "bulkhead", "tokens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"PLAIN":      "abc",
		"QUOTE":      `It's "me" & you`,
		"SPACES":     "two  spaces  here",
		"lower_case": "ok",
		"_UNDER":     "1",
		"DOLLAR":     "$(echo run) `echo run` $HOME ${PATH}",
		"UNICODE":    "日本語 の値",
		"EQUALS":     "a=b=c",
		"QUOTES":     "'''",
		"BACKSLASH":  `a\nb \' c`,
	}
	var file strings.Builder
	for name, value := range values {
		file.WriteString(name + "=" + value + "\n")
	}
	// What must not come through: not a name, a control character, the
	// GitHub token's name.
	file.WriteString("9DIGIT=x\nWITH-DASH=x\nTAB=a\tb\nGH_TOKEN=shadow\nEMPTY=\n")
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(file.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	clean := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "BULKHEAD_GUEST_ROOT=" + root}
	guest := exec.Command(sh, "dev/bh-guest", "env")
	guest.Env = clean
	exports, err := guest.Output()
	if err != nil {
		t.Fatalf("bh-guest env: %v", err)
	}
	for _, name := range []string{"9DIGIT", "WITH-DASH", "TAB", "GH_TOKEN", "EMPTY"} {
		if strings.Contains(string(exports), "export "+name+"=") {
			t.Errorf("%s was exported:\n%s", name, exports)
		}
	}

	// Evaluated by a shell, each variable is the value, byte for byte.
	for name, want := range values {
		show := exec.Command(sh, "-c", string(exports)+"\nprintf '%s' \"$"+name+"\"")
		if strings.HasPrefix(name, "lower") || strings.HasPrefix(name, "_") {
			show = exec.Command(sh, "-c", string(exports)+"\nprintf '%s' \"${"+name+"}\"")
		}
		show.Env = clean
		got, err := show.Output()
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
}
