//go:build unix

package gates

import "golang.org/x/sys/unix"

// statfsUsage reports byte and inode usage percentages for the filesystem
// backing path. Byte usage follows df's convention — used / (used + available
// to unprivileged writers) — because the reserved-block pool is not space a
// Mills write can actually consume.
//
// Filesystems that do not track inodes (overlayfs, some tmpfs configurations)
// report zero total inodes; inode usage is then reported as 0 rather than a
// divide-by-zero, and only byte usage constrains the verdict.
func statfsUsage(path string) (capacityUsedPercent, inodeUsedPercent float64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}

	if st.Blocks > 0 {
		used := float64(st.Blocks - st.Bfree)
		usable := used + float64(st.Bavail)
		if usable > 0 {
			capacityUsedPercent = used / usable * 100
		}
	}
	if st.Files > 0 {
		inodeUsedPercent = float64(st.Files-st.Ffree) / float64(st.Files) * 100
	}
	return capacityUsedPercent, inodeUsedPercent, nil
}
