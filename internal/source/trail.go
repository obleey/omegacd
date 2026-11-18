package source

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/obleey/omegacd/internal/logging"
)

// A Sync's trail through a standalone Source Server (Q188): the Control Plane sends the app and Sync Operation its
// context carries with each call, and the Source Server's log lines carry them, so op=… reads through every log.
// In-process the context itself flows into the Source Server.
var trailKeys = []string{"app", "op"}

func withTrail(ctx context.Context) context.Context {
	for _, k := range trailKeys {
		if v, ok := logging.Value(ctx, k); ok {
			ctx = metadata.AppendToOutgoingContext(ctx, "omegacd-"+k, fmt.Sprint(v))
		}
	}
	return ctx
}

func sendTrail(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(withTrail(ctx), method, req, reply, cc, opts...)
}

func sendTrailStream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(withTrail(ctx), desc, cc, method, opts...)
}

func fromTrail(ctx context.Context) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, k := range trailKeys {
		if v := md.Get("omegacd-" + k); len(v) > 0 {
			ctx = logging.With(ctx, k, v[0])
		}
	}
	return ctx
}

// ReceiveTrail and ReceiveTrailStream put a call's trail in its context, for the Source Server's log lines.
func ReceiveTrail(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(fromTrail(ctx), req)
}

func ReceiveTrailStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, trailStream{ss, fromTrail(ss.Context())})
}

type trailStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s trailStream) Context() context.Context { return s.ctx }
