package engine

import (
	"strings"
	"testing"
)

func TestParseSocketProbe(t *testing.T) {
	if gid, err := ParseSocketProbe("socket 998\n"); err != nil || gid != 998 {
		t.Errorf("socket 998: %d, %v", gid, err)
	}
	if gid, err := ParseSocketProbe("socket 0"); err != nil || gid != 0 {
		t.Errorf("socket 0 (Docker Desktop): %d, %v", gid, err)
	}
	// A bind of a path that does not exist can come up as an empty
	// directory; its group 0 must not pass for the socket's.
	if _, err := ParseSocketProbe("directory 0"); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a directory: %v", err)
	}
	for _, output := range []string{"", "socket", "socket x", "998"} {
		if _, err := ParseSocketProbe(output); err == nil {
			t.Errorf("%q was accepted", output)
		}
	}
}
