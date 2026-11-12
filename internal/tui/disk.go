package tui

import (
	"fmt"
	"strings"

	"github.com/ys319/bulkhead/internal/plan"
)

// humanBytes writes a size the way docker does: decimal units, a digit
// after the point only for small numbers.
func humanBytes(size int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	value, unit := float64(size), 0
	for value >= 1000 && unit < len(units)-1 {
		value, unit = value/1000, unit+1
	}
	if unit == 0 || value >= 10 {
		return fmt.Sprintf("%.0f%s", value, units[unit])
	}
	return fmt.Sprintf("%.1f%s", value, units[unit])
}

// DiskSummary is what an environment's volumes take: the total of all of
// them, and the three that hold data one by one. ok is false when none of
// its volumes is among the sizes (it was never started).
func DiskSummary(id string, sizes map[string]int64) (summary string, ok bool) {
	var total int64
	for _, key := range plan.AllVolumeKeys() {
		if size, found := sizes[plan.Volume(id, key)]; found {
			total, ok = total+size, true
		}
	}
	if !ok {
		return "", false
	}
	var parts []string
	for _, key := range []string{"workspace", "home", "nix"} {
		if size, found := sizes[plan.Volume(id, key)]; found {
			parts = append(parts, key+" "+humanBytes(size))
		}
	}
	return humanBytes(total) + " (" + strings.Join(parts, ", ") + ")", true
}
