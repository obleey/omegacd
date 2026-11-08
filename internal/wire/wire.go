// Package wire is the Agent ↔ Control Plane connection (Q16, Q20, Q51): one WebSocket the Agent opens, binary frames,
// each a protobuf Envelope (proto/omegacd/agent/v1). Signals, status and logs go before queued bulk data, so large
// blobs, sent in chunks, never hold them up; pings keep idle proxies from cutting a quiet connection; a blob cut off by
// a dropped connection resumes where it stopped (Assembler.Offset, BlobRequest.offset).
package wire

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

const (
	// PingInterval keeps a quiet connection alive through proxies that cut idle ones after 60 s (Q51).
	PingInterval = 30 * time.Second
	// ChunkSize is the most of a blob one frame carries: a status or a log line waits for one chunk at most.
	ChunkSize = 64 << 10
	// maxFrame bounds what one frame may be: a chunk, or a Desired State listing many files.
	maxFrame = 8 << 20
	// writeTimeout bounds one frame's write: a peer that stopped reading ends the connection instead of hanging it.
	writeTimeout = 30 * time.Second
)

// Conn is one live connection. Send and SendBlob may be called concurrently; Recv by one reader at a time, which must
// keep reading: pings are answered by the reads.
type Conn struct {
	ws      *websocket.Conn
	control chan *agentv1.Envelope // everything but blob chunks, sent first
	bulk    chan *agentv1.Envelope // blob chunks
	nextID  atomic.Uint64
	done    chan struct{}
	once    sync.Once
	err     error
}

func newConn(ws *websocket.Conn, ping time.Duration) *Conn {
	ws.SetReadLimit(maxFrame)
	c := &Conn{ws: ws, control: make(chan *agentv1.Envelope, 64), bulk: make(chan *agentv1.Envelope, 4), done: make(chan struct{})}
	go c.write()
	go c.ping(ping)
	return c
}

// Accept upgrades an HTTP request to a connection: the Control Plane's side.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, err
	}
	return newConn(ws, PingInterval), nil
}

// Dial opens a connection to url (ws:// or wss://, also http:// and https://): the Agent's side.
func Dial(ctx context.Context, url string, header http.Header) (*Conn, error) {
	return dial(ctx, url, header, PingInterval)
}

func dial(ctx context.Context, url string, header http.Header, ping time.Duration) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	return newConn(ws, ping), nil
}

// Send queues e (its id set if unset); it returns once e is queued, or the connection is gone.
func (c *Conn) Send(ctx context.Context, e *agentv1.Envelope) error {
	if e.Id == 0 {
		e.Id = c.nextID.Add(1)
	}
	q := c.control
	if e.GetBlobChunk() != nil {
		q = c.bulk
	}
	select {
	case q <- e:
		return nil
	case <-c.done:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendBlob queues data's chunks from offset (a resumed transfer), at least one even for an empty blob: the receiver
// knows a blob is complete when what it got hashes to its sha256.
func (c *Conn) SendBlob(ctx context.Context, sha256 []byte, data []byte, offset uint64) error {
	if offset > uint64(len(data)) {
		return fmt.Errorf("blob %x: offset %d past its %d bytes", sha256, offset, len(data))
	}
	for {
		end := min(offset+ChunkSize, uint64(len(data)))
		chunk := &agentv1.BlobChunk{Sha256: sha256, Offset: offset, Data: data[offset:end]}
		if err := c.Send(ctx, &agentv1.Envelope{Body: &agentv1.Envelope_BlobChunk{BlobChunk: chunk}}); err != nil {
			return err
		}
		if offset = end; offset == uint64(len(data)) {
			return nil
		}
	}
}

// Recv returns the next envelope from the peer.
func (c *Conn) Recv(ctx context.Context) (*agentv1.Envelope, error) {
	typ, b, err := c.ws.Read(ctx)
	if err != nil {
		c.fail(err)
		return nil, err
	}
	if typ != websocket.MessageBinary {
		err := errors.New("wire: a text frame: want binary protobuf")
		c.fail(err)
		return nil, err
	}
	e := &agentv1.Envelope{}
	if err := proto.Unmarshal(b, e); err != nil {
		c.fail(err)
		return nil, fmt.Errorf("wire: %w", err)
	}
	return e, nil
}

// Close ends the connection.
func (c *Conn) Close() error {
	c.fail(errClosed)
	return nil
}

var errClosed = errors.New("wire: closed")

// Done is closed once the connection has ended; Err says why.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	<-c.done
	return c.err
}

func (c *Conn) fail(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.done)
		c.ws.CloseNow()
	})
}

// write sends queued envelopes, control ones first: a blob chunk goes only while no control message waits.
func (c *Conn) write() {
	for {
		var e *agentv1.Envelope
		select {
		case e = <-c.control:
		default:
			select {
			case e = <-c.control:
			case e = <-c.bulk:
			case <-c.done:
				return
			}
		}
		b, err := proto.Marshal(e)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err = c.ws.Write(ctx, websocket.MessageBinary, b)
			cancel()
		}
		if err != nil {
			c.fail(fmt.Errorf("wire: write: %w", err))
			return
		}
	}
}

// ping checks the peer every interval; one that doesn't answer within it ends the connection.
func (c *Conn) ping(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		err := c.ws.Ping(ctx)
		cancel()
		if err != nil {
			c.fail(fmt.Errorf("wire: no answer to a ping: %w", err))
			return
		}
	}
}

// Backoff is the wait before reconnect attempt n (from 0): doubling from 1 s up to a minute, ±20 % jitter so the
// Agents behind one restarted Control Plane don't all come back at once.
func Backoff(n int) time.Duration {
	d := time.Minute
	if n < 6 {
		d = time.Second << n
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

var backoff = Backoff // tests wait less

// Keep holds a connection open until ctx ends: it dials, serves it until it ends, and dials again after Backoff. A
// connection that lasted a minute starts the backoff over.
func Keep(ctx context.Context, dial func(context.Context) (*Conn, error), serve func(context.Context, *Conn) error, onDown func(error)) {
	for n := 0; ctx.Err() == nil; {
		c, err := dial(ctx)
		if err == nil {
			start := time.Now()
			err = serve(ctx, c)
			c.Close()
			if time.Since(start) > time.Minute {
				n = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		if onDown != nil {
			onDown(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff(n)):
		}
		n++
	}
}
