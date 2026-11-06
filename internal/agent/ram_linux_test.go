package agent

import (
	"os"
	"testing"
)

func TestOnRAM(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no /dev/shm")
	}
	if !onRAM("/dev/shm") {
		t.Error("/dev/shm (tmpfs) doesn't count as RAM")
	}
	if onRAM("/") && os.Getenv("CI") != "" {
		t.Error("/ counts as RAM on a CI runner")
	}
}
