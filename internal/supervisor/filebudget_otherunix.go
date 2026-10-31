//go:build unix && !darwin

package supervisor

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func allocatedRegularBytes(_ string, st unix.Stat_t) (uint64, error) {
	if st.Blocks < 0 {
		return 0, fmt.Errorf("negative block count")
	}
	blocks := uint64(st.Blocks)
	if blocks > math.MaxUint64/512 {
		return 0, fmt.Errorf("allocated block overflow")
	}
	return blocks * 512, nil
}
