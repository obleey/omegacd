package agent

import "golang.org/x/sys/unix"

// onRAM reports whether dir is on a RAM filesystem, where decrypted secrets never reach a disk.
func onRAM(dir string) bool {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return false
	}
	return st.Type == unix.TMPFS_MAGIC || st.Type == unix.RAMFS_MAGIC
}
