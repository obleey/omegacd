package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/obleey/omegacd/internal/blob"
	"github.com/obleey/omegacd/internal/domain"
	sourcev1 "github.com/obleey/omegacd/internal/proto/source/v1"
)

// Serve registers s as the source/v1 service on g.
func Serve(g *grpc.Server, s domain.Source) {
	sourcev1.RegisterSourceServiceServer(g, &grpcServer{s: s})
}

type grpcServer struct {
	sourcev1.UnimplementedSourceServiceServer
	s domain.Source
}

// toStatus keeps the domain errors callers test for across the wire.
func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domain.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, err.Error())
	}
	return status.Error(codes.Unknown, err.Error())
}

func (g *grpcServer) Resolve(ctx context.Context, r *sourcev1.ResolveRequest) (*sourcev1.ResolveResponse, error) {
	rev, err := g.s.Resolve(ctx, r.RepoUrl, r.Ref)
	return &sourcev1.ResolveResponse{Revision: rev}, toStatus(err)
}

func (g *grpcServer) Invalidate(_ context.Context, r *sourcev1.InvalidateRequest) (*sourcev1.InvalidateResponse, error) {
	g.s.Invalidate(r.RepoUrl)
	return &sourcev1.InvalidateResponse{}, nil
}

func (g *grpcServer) Render(ctx context.Context, r *sourcev1.RenderRequest) (*sourcev1.RenderResponse, error) {
	out, err := g.s.Render(ctx, domain.RenderSpec{RepoURL: r.RepoUrl, Revision: r.Revision, Path: r.Path,
		ComposeFiles: r.ComposeFiles, Profiles: r.Profiles, Apps: r.Apps})
	if err != nil {
		return nil, toStatus(err)
	}
	resp := &sourcev1.RenderResponse{Checksum: out.Checksum, Warnings: out.Warnings}
	if out.Invalid != nil {
		resp.Invalid = out.Invalid.Error()
	}
	for _, f := range out.Files {
		resp.Files = append(resp.Files, &sourcev1.File{Path: f.Path, Mode: uint32(f.Mode), Sha256: f.SHA256})
	}
	for _, a := range out.Apps {
		resp.Apps = append(resp.Apps, &sourcev1.AppDefinition{Name: a.Name, Kind: a.Kind, RepoUrl: a.RepoURL, Path: a.Path,
			TargetRevision: a.TargetRevision, ComposeFiles: a.ComposeFiles, Profiles: a.Profiles, Host: a.Host,
			AutoSync: a.AutoSync, SelfHeal: a.SelfHeal, Prune: a.Prune, PruneVolumes: a.PruneVolumes, Adopt: a.Adopt})
	}
	return resp, nil
}

const chunkSize = 1 << 20

func (g *grpcServer) Blob(r *sourcev1.BlobRequest, stream grpc.ServerStreamingServer[sourcev1.BlobResponse]) error {
	b, err := g.s.Blob(stream.Context(), r.Sha256)
	if err != nil {
		return toStatus(err)
	}
	for len(b) > 0 {
		n := min(chunkSize, len(b))
		if err := stream.Send(&sourcev1.BlobResponse{Data: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (g *grpcServer) Diff(ctx context.Context, r *sourcev1.DiffRequest) (*sourcev1.DiffResponse, error) {
	d, err := g.s.Diff(ctx, r.RepoUrl, r.FromRevision, r.ToRevision, r.Path)
	return &sourcev1.DiffResponse{Diff: d}, toStatus(err)
}

func (g *grpcServer) CheckAccess(ctx context.Context, r *sourcev1.CheckAccessRequest) (*sourcev1.CheckAccessResponse, error) {
	a, err := g.s.CheckAccess(ctx, r.RepoUrl)
	return &sourcev1.CheckAccessResponse{Access: a.Access, CredentialPrefix: a.CredentialPrefix,
		DefaultBranch: a.DefaultBranch, Message: a.Message}, toStatus(err)
}

func (g *grpcServer) ListCredentials(ctx context.Context, _ *sourcev1.ListCredentialsRequest) (*sourcev1.ListCredentialsResponse, error) {
	cs, err := g.s.ListCredentials(ctx)
	resp := &sourcev1.ListCredentialsResponse{}
	for _, c := range cs {
		resp.Credentials = append(resp.Credentials, toCredential(c))
	}
	return resp, toStatus(err)
}

func (g *grpcServer) PutCredential(ctx context.Context, r *sourcev1.PutCredentialRequest) (*sourcev1.PutCredentialResponse, error) {
	c := r.GetCredential()
	saved, err := g.s.SaveCredential(ctx, domain.RepoCredential{ID: c.GetId(), URLPrefix: c.GetUrlPrefix(),
		Username: c.GetUsername(), Password: c.GetPassword(), SSHPrivateKey: c.GetSshPrivateKey()})
	if err != nil {
		return nil, toStatus(err)
	}
	return &sourcev1.PutCredentialResponse{Credential: toCredential(saved)}, nil
}

func (g *grpcServer) DeleteCredential(ctx context.Context, r *sourcev1.DeleteCredentialRequest) (*sourcev1.DeleteCredentialResponse, error) {
	return &sourcev1.DeleteCredentialResponse{}, toStatus(g.s.DeleteCredential(ctx, r.Id))
}

// toCredential never carries secrets: they only go to the Source Server.
func toCredential(c domain.RepoCredential) *sourcev1.Credential {
	return &sourcev1.Credential{Id: c.ID, UrlPrefix: c.URLPrefix, Username: c.Username, HasPassword: c.HasPassword, HasSshKey: c.HasSSHKey}
}

func fromCredential(c *sourcev1.Credential) domain.RepoCredential {
	return domain.RepoCredential{ID: c.Id, URLPrefix: c.UrlPrefix, Username: c.Username, HasPassword: c.HasPassword, HasSSHKey: c.HasSshKey}
}

// Client is a standalone Source Server, over gRPC with mTLS.
type Client struct {
	conn *grpc.ClientConn
	c    sourcev1.SourceServiceClient
}

var _ domain.Source = (*Client)(nil)

// Dial connects to the Source Server at rawURL (https://host:port) with the client certificate in tlsDir.
func Dial(rawURL, tlsDir string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() == "" {
		return nil, fmt.Errorf("OMEGACD_SOURCE_SERVER=%q: want https://host:port", rawURL)
	}
	cfg, err := ClientTLS(tlsDir, u.Hostname())
	if err != nil {
		return nil, err
	}
	// a restarted Source Server is found within seconds, not after gRPC's default backoff of up to two minutes (Q124)
	conn, err := grpc.NewClient(u.Host, grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.Config{BaseDelay: time.Second, Multiplier: 1.6,
			Jitter: 0.2, MaxDelay: 5 * time.Second}, MinConnectTimeout: 5 * time.Second}))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, c: sourcev1.NewSourceServiceClient(conn)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// remoteErr is an error from the Source Server, still matching ErrNotFound, ErrInvalid or ErrAlreadyExists.
type remoteErr struct {
	msg string
	is  error
}

func (e remoteErr) Error() string { return e.msg }
func (e remoteErr) Unwrap() error { return e.is }

func fromStatus(err error) error {
	st, ok := status.FromError(err)
	switch {
	case err == nil:
		return nil
	case !ok:
		return err
	case st.Code() == codes.NotFound:
		return remoteErr{st.Message(), domain.ErrNotFound}
	case st.Code() == codes.InvalidArgument:
		return remoteErr{st.Message(), domain.ErrInvalid}
	case st.Code() == codes.AlreadyExists:
		return remoteErr{st.Message(), domain.ErrAlreadyExists}
	case st.Code() == codes.Unavailable:
		return remoteErr{"Source Server unreachable (OMEGACD_SOURCE_SERVER): " + st.Message(), domain.ErrUnavailable}
	}
	return fmt.Errorf("source server: %s", st.Message())
}

func (c *Client) Resolve(ctx context.Context, repoURL, rev string) (string, error) {
	r, err := c.c.Resolve(ctx, &sourcev1.ResolveRequest{RepoUrl: repoURL, Ref: rev})
	return r.GetRevision(), fromStatus(err)
}

// Invalidate is fire and forget, like the in-process one: the next Resolve fetches, or fails and says why.
func (c *Client) Invalidate(repoURL string) {
	c.c.Invalidate(context.Background(), &sourcev1.InvalidateRequest{RepoUrl: repoURL})
}

func (c *Client) Render(ctx context.Context, spec domain.RenderSpec) (domain.Rendered, error) {
	r, err := c.c.Render(ctx, &sourcev1.RenderRequest{RepoUrl: spec.RepoURL, Revision: spec.Revision, Path: spec.Path,
		ComposeFiles: spec.ComposeFiles, Profiles: spec.Profiles, Apps: spec.Apps})
	if err != nil {
		return domain.Rendered{}, fromStatus(err)
	}
	out := domain.Rendered{Checksum: r.Checksum, Warnings: r.Warnings}
	if r.Invalid != "" {
		out.Invalid = remoteErr{r.Invalid, domain.ErrInvalid}
	}
	for _, f := range r.Files {
		out.Files = append(out.Files, domain.File{Path: f.Path, Mode: fs.FileMode(f.Mode), SHA256: f.Sha256})
	}
	for _, a := range r.Apps {
		out.Apps = append(out.Apps, domain.Application{Name: a.Name, Kind: a.Kind, RepoURL: a.RepoUrl, Path: a.Path,
			TargetRevision: a.TargetRevision, ComposeFiles: a.ComposeFiles, Profiles: a.Profiles, Host: a.Host,
			AutoSync: a.AutoSync, SelfHeal: a.SelfHeal, Prune: a.Prune, PruneVolumes: a.PruneVolumes, Adopt: a.Adopt})
	}
	return out, nil
}

func (c *Client) Blob(ctx context.Context, hash string) ([]byte, error) {
	stream, err := c.c.Blob(ctx, &sourcev1.BlobRequest{Sha256: hash})
	if err != nil {
		return nil, fromStatus(err)
	}
	var b bytes.Buffer
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fromStatus(err)
		}
		b.Write(r.Data)
	}
	if blob.Hash(b.Bytes()) != hash {
		return nil, fmt.Errorf("blob %s from the source server: content does not match", hash)
	}
	return b.Bytes(), nil
}

func (c *Client) Diff(ctx context.Context, repoURL, fromSHA, toSHA, path string) (string, error) {
	r, err := c.c.Diff(ctx, &sourcev1.DiffRequest{RepoUrl: repoURL, FromRevision: fromSHA, ToRevision: toSHA, Path: path})
	return r.GetDiff(), fromStatus(err)
}

func (c *Client) CheckAccess(ctx context.Context, repoURL string) (domain.RepoAccess, error) {
	r, err := c.c.CheckAccess(ctx, &sourcev1.CheckAccessRequest{RepoUrl: repoURL})
	if err != nil {
		return domain.RepoAccess{}, fromStatus(err)
	}
	return domain.RepoAccess{Access: r.Access, CredentialPrefix: r.CredentialPrefix, DefaultBranch: r.DefaultBranch, Message: r.Message}, nil
}

func (c *Client) ListCredentials(ctx context.Context) ([]domain.RepoCredential, error) {
	r, err := c.c.ListCredentials(ctx, &sourcev1.ListCredentialsRequest{})
	if err != nil {
		return nil, fromStatus(err)
	}
	out := []domain.RepoCredential{}
	for _, cr := range r.Credentials {
		out = append(out, fromCredential(cr))
	}
	return out, nil
}

func (c *Client) SaveCredential(ctx context.Context, cred domain.RepoCredential) (domain.RepoCredential, error) {
	r, err := c.c.PutCredential(ctx, &sourcev1.PutCredentialRequest{Credential: &sourcev1.Credential{Id: cred.ID,
		UrlPrefix: cred.URLPrefix, Username: cred.Username, Password: cred.Password, SshPrivateKey: cred.SSHPrivateKey}})
	if err != nil {
		return domain.RepoCredential{}, fromStatus(err)
	}
	return fromCredential(r.Credential), nil
}

func (c *Client) DeleteCredential(ctx context.Context, id int64) error {
	_, err := c.c.DeleteCredential(ctx, &sourcev1.DeleteCredentialRequest{Id: id})
	return fromStatus(err)
}
