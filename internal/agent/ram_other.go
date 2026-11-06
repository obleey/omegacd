//go:build !linux

package agent

// onRAM: Hosts are Linux; elsewhere (a developer's Mac) the work dir counts as disk.
func onRAM(string) bool { return false }
