package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

type Config struct {
	Hosts          func(ctx context.Context, name string) (domain.Host, error) // store.GetHost; never called for domain.LocalHost
	ListHosts      func(ctx context.Context) ([]domain.Host, error)            // store.ListHosts (excludes local)
	KnownHostsFile string                                                      // OpenSSH known_hosts for ssh:// hosts; ssh refused if empty or missing
	KeyDir         string                                                      // dir where SSH keys / TLS PEMs are written with 0600 for the ssh/tls clients
	NoLocalHost    bool                                                        // no local Docker engine (e.g. in Kubernetes): only remote Hosts
}

// hostConn is a Docker CLI and Compose service bound to one Host.
type hostConn struct {
	host    domain.Host
	cli     command.Cli
	compose api.Compose
}

// persistent, so bind-mounted files survive a reboot of the Host
const defaultWorkDir = "/var/lib/omegacd"

func hostName(h string) string { return cmp.Or(h, domain.LocalHost) }

// remoteAppDir is where an Application's files live on a non-local Host.
func remoteAppDir(h domain.Host, app string) string {
	return path.Join(cmp.Or(h.WorkDir, defaultWorkDir), "apps", app)
}

// conn returns the cached connection to host ("" = LocalHost), creating it on first use.
func (e *Engine) conn(ctx context.Context, host string) (*hostConn, error) {
	host = hostName(host)
	e.mu.Lock()
	hc := e.conns[host]
	e.mu.Unlock()
	if hc != nil {
		return hc, nil
	}
	if host == domain.LocalHost {
		if e.cfg.NoLocalHost {
			return nil, domain.ErrLocalHostDisabled
		}
		return nil, errors.New("local docker host not connected") // created in New, only gone after Close
	}
	if e.cfg.Hosts == nil {
		return nil, fmt.Errorf("host %q: %w", host, domain.ErrNotFound)
	}
	h, err := e.cfg.Hosts(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("host %q: %w", host, err)
	}
	hc, err = e.dial(h)
	if err != nil {
		return nil, fmt.Errorf("host %q: %w", host, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur := e.conns[host]; cur != nil { // lost a race with another caller
		hc.cli.Client().Close()
		return cur, nil
	}
	e.conns[host] = hc
	return hc, nil
}

// dial builds an API client for a remote Host. Nothing is contacted yet.
func (e *Engine) dial(h domain.Host) (*hostConn, error) {
	if h.Name == "" || h.Name != filepath.Base(h.Name) || h.Name == ".." {
		return nil, fmt.Errorf("%w: host name %q", domain.ErrInvalid, h.Name)
	}
	u, err := url.Parse(h.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: host url: %v", domain.ErrInvalid, err)
	}
	var opts []client.Opt
	switch u.Scheme {
	case "ssh":
		keyFile := ""
		if h.SSHPrivateKey != "" {
			key := h.SSHPrivateKey
			if !strings.HasSuffix(key, "\n") {
				key += "\n" // OpenSSH rejects keys without the trailing newline
			}
			if keyFile, err = e.writeKey(h.Name, "id", key); err != nil {
				return nil, err
			}
		}
		sshFlags, err := sshFlags(keyFile, e.cfg.KnownHostsFile)
		if err != nil {
			return nil, err
		}
		helper, err := connhelper.GetConnectionHelperWithSSHOpts(h.URL, sshFlags)
		if err != nil {
			return nil, err
		}
		opts = []client.Opt{
			client.WithHTTPClient(&http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		}
	case "tcp":
		opts = []client.Opt{client.WithHost(h.URL)}
		if h.TLSCert != "" && h.TLSKey != "" {
			var ca string
			if h.TLSCA != "" {
				if ca, err = e.writeKey(h.Name, "ca.pem", h.TLSCA); err != nil {
					return nil, err
				}
			}
			cert, err := e.writeKey(h.Name, "cert.pem", h.TLSCert)
			if err != nil {
				return nil, err
			}
			key, err := e.writeKey(h.Name, "key.pem", h.TLSKey)
			if err != nil {
				return nil, err
			}
			opts = append(opts, client.WithTLSClientConfig(ca, cert, key))
		} else {
			slog.Warn("docker host without TLS: anyone on the network can control it", "host", h.Name, "url", h.URL)
		}
	default:
		return nil, fmt.Errorf("%w: host url %q: scheme must be ssh:// or tcp://", domain.ErrInvalid, h.URL)
	}
	c, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	dcli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("docker cli: %w", err)
	}
	if err := dcli.Initialize(&flags.ClientOptions{Hosts: []string{h.URL}}, command.WithAPIClient(c)); err != nil {
		return nil, fmt.Errorf("docker cli: %w", err)
	}
	svc, err := newCompose(dcli, nil)
	if err != nil {
		return nil, err
	}
	return &hostConn{host: h, cli: dcli, compose: svc}, nil
}

// sshFlags makes ssh use only omegacd's key and known_hosts, never an agent, ~/.ssh or a prompt.
func sshFlags(keyFile, knownHosts string) ([]string, error) {
	if knownHosts == "" {
		return nil, errors.New("ssh: no known_hosts file configured (OMEGACD_SSH_KNOWN_HOSTS)")
	}
	if _, err := os.Stat(knownHosts); err != nil {
		return nil, fmt.Errorf("ssh: known_hosts file: %w", err)
	}
	var f []string
	if keyFile != "" {
		f = append(f, "-i", keyFile)
	}
	return append(f,
		"-o", "UserKnownHostsFile="+knownHosts,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes"), nil
}

// writeKey writes a credential to KeyDir/<host>/<name> with mode 0600 and returns its path.
func (e *Engine) writeKey(host, name, content string) (string, error) {
	if e.cfg.KeyDir == "" {
		return "", errors.New("no key directory configured for host credentials")
	}
	dir := filepath.Join(e.cfg.KeyDir, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil { // in case it existed with a wider mode
		f.Close()
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}

func (e *Engine) HostChanged(host string) {
	host = hostName(host)
	if host == domain.LocalHost {
		return
	}
	e.mu.Lock()
	hc := e.conns[host]
	delete(e.conns, host)
	e.mu.Unlock()
	if hc != nil {
		hc.cli.Client().Close()
	}
}

func (e *Engine) HostStatus(ctx context.Context, host string) domain.HostStatus {
	st := domain.HostStatus{Name: hostName(host)}
	hc, err := e.conn(ctx, host)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := hc.cli.Client().Info(ctx, client.InfoOptions{})
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Connected = true
	st.ServerVersion = res.Info.ServerVersion
	st.OS = res.Info.OperatingSystem
	st.Containers = res.Info.Containers
	return st
}
