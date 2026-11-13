package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// backup writes the Control Plane's backup (Q114) to a new file, owner-only: the database and the encrypted Git
// credentials. The keys aren't in it (Q115): it says which to keep apart.
func (c *CLI) backup(ctx context.Context, args []string) error {
	var o opts
	pos, err := parse(c.flags("backup", &o), args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := c.client(o)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(pos[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup file: %w (it is never overwritten)", err)
	}
	resp, err := cl.request(ctx, "GET", "/api/backup", nil)
	if err == nil {
		_, err = io.Copy(f, resp.Body)
		resp.Body.Close()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(pos[0])
		return err
	}
	fmt.Fprintf(c.Stdout, "backup written to %s: the database and the encrypted Git credentials.\n"+
		"Not in it, keep them apart: the Master Key (OMEGACD_AGE_KEY_FILE; without it, enter the registry credentials "+
		"again) and the Source Server's source.key (without it, enter the Git credentials again).\n", pos[0])
	return nil
}
