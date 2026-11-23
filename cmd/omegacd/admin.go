package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/obleey/omegacd/internal/api"
	"github.com/obleey/omegacd/internal/crypto"
	"github.com/obleey/omegacd/internal/store"
)

// resetAdminPassword is omegacd reset-admin-password, run on the Control Plane's machine with its settings (Q311): a
// new generated password in the data dir's initial-admin-password, to be changed at the next login. It opens the
// database beside a running server, as omegacd backup's copy does; it never makes a Master Key.
func resetAdminPassword(ctx context.Context, out io.Writer) error {
	dataDir := cmp.Or(os.Getenv("OMEGACD_DATA"), "/data")
	ageKeyFile := cmp.Or(os.Getenv("OMEGACD_AGE_KEY_FILE"), "/etc/omegacd/age.key")
	id, err := crypto.LoadIdentity(ageKeyFile)
	if err != nil {
		return fmt.Errorf("master key: %w (OMEGACD_AGE_KEY_FILE; run this where the server runs, with its settings)", err)
	}
	db := filepath.Join(dataDir, "omegacd.db")
	if _, err := os.Stat(db); err != nil {
		return fmt.Errorf("no OmegaCD database: %w (OMEGACD_DATA; run this where the server runs, with its settings)", err)
	}
	if err := notRootOver(db); err != nil {
		return err
	}
	st, err := store.Open(db, crypto.NewSealer(id))
	if err != nil {
		return err
	}
	defer st.Close()
	file := filepath.Join(dataDir, api.InitialPasswordFile)
	if err := api.ResetAdminPassword(ctx, st, file); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "A new admin password is in %s: log in as admin and change it. Every session has ended; API tokens still work.\n", file)
	return err
}

// geteuid is os.Geteuid; tests replace it.
var geteuid = os.Geteuid

// notRootOver refuses root on a database another user owns (Q316): SQLite's -wal and -shm files and the password file
// would be root's, and the server could no longer write its database or delete the file.
func notRootOver(db string) error {
	st, err := os.Stat(db)
	if err != nil { // coverage: unreachable, the caller just found it
		return err
	}
	if owner := st.Sys().(*syscall.Stat_t).Uid; geteuid() == 0 && owner != 0 {
		return fmt.Errorf("run it as the server's user (uid %d), not root, or its files become root's; with the .deb: sudo omegacd-reset-admin-password", owner)
	}
	return nil
}
