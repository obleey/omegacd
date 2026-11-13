package wire

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// serve runs a test server whose connections handle runs on; url is its ws:// address.
func serve(t *testing.T, handle func(*Conn)) (url string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		handle(c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func hello(v string) *agentv1.Envelope {
	return &agentv1.Envelope{Body: &agentv1.Envelope_Hello{Hello: &agentv1.Hello{AgentVersion: v}}}
}

func TestRoundTrip(t *testing.T) {
	ctx := t.Context()
	url := serve(t, func(c *Conn) { // echoes, answering each envelope
		for {
			e, err := c.Recv(ctx)
			if err != nil {
				return
			}
			c.Send(ctx, &agentv1.Envelope{ReplyTo: e.Id, Body: e.Body})
		}
	})
	c, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, v := range []string{"0.2.0", "0.2.1"} {
		e := hello(v)
		if err := c.Send(ctx, e); err != nil {
			t.Fatal(err)
		}
		got, err := c.Recv(ctx)
		if err != nil || got.ReplyTo != e.Id || got.GetHello().GetAgentVersion() != v {
			t.Fatalf("got %v, %v; want the answer to %d", got, err, e.Id)
		}
	}
}

// Status goes before queued blob chunks: sent while a large blob is stuck (the peer isn't reading yet), it overtakes
// every chunk still waiting and arrives long before the blob's end.
func TestControlBeforeBulk(t *testing.T) {
	ctx := t.Context()
	const chunks = 512 // 32 MB: more than the sockets buffer, so the writer gets stuck
	reading := make(chan struct{})
	order := make(chan int, 1)
	url := serve(t, func(c *Conn) {
		<-reading
		for i := 0; ; i++ {
			e, err := c.Recv(ctx)
			if err != nil {
				return
			}
			if e.GetHello() != nil {
				order <- i
				return
			}
		}
	})
	c, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	blob := make([]byte, chunks*ChunkSize)
	sum := sha256.Sum256(blob)
	go c.SendBlob(ctx, sum[:], blob, 0)
	time.Sleep(300 * time.Millisecond) // the buffers fill, the writer and SendBlob wait
	c.Send(ctx, hello("status"))
	close(reading)
	select {
	case i := <-order:
		if i > chunks/2 { // the chunks already in the buffers and the queue (a few MB) come first; the rest after
			t.Fatalf("the status came after %d of %d chunks", i, chunks)
		}
		t.Logf("the status overtook %d of %d chunks", chunks-i, chunks)
	case <-time.After(30 * time.Second):
		t.Fatal("the status never came")
	}
}

// A blob cut off by a dropped connection resumes from where it stopped on the next one, and arrives whole.
func TestBlobResumesAfterDrop(t *testing.T) {
	ctx := t.Context()
	blob := make([]byte, 10*ChunkSize+123)
	rand.Read(blob)
	sum := sha256.Sum256(blob)
	var asm Assembler
	got := make(chan []byte, 1)
	var conns atomic.Int32
	url := serve(t, func(c *Conn) {
		first := conns.Add(1) == 1
		for n := 0; ; n++ {
			e, err := c.Recv(ctx)
			if err != nil {
				return
			}
			if first && n == 4 { // the connection drops after 4 chunks
				c.Close()
				return
			}
			if b, done, err := asm.Add(e.GetBlobChunk()); err != nil {
				t.Error(err)
			} else if done {
				got <- bytes.Clone(b)
			}
		}
	})
	c, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SendBlob(ctx, sum[:], blob, 0)
	<-c.Done()
	from := asm.Offset(sum[:])
	if from != 4*ChunkSize {
		t.Fatalf("kept %d bytes, want the 4 chunks before the drop", from)
	}
	c, err = Dial(ctx, url, nil) // reconnected: what was missing, from the offset
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SendBlob(ctx, sum[:], blob, from); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if !bytes.Equal(b, blob) {
			t.Fatal("the blob arrived changed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the blob never completed")
	}
}

func TestAssembler(t *testing.T) {
	var a Assembler
	empty := sha256.Sum256(nil)
	if b, done, err := a.Add(&agentv1.BlobChunk{Sha256: empty[:]}); err != nil || !done || len(b) != 0 {
		t.Fatalf("an empty blob: %q %v %v", b, done, err)
	}
	sum := sha256.Sum256([]byte("abcdef"))
	if _, done, err := a.Add(&agentv1.BlobChunk{Sha256: sum[:], Data: []byte("abc")}); err != nil || done {
		t.Fatalf("half: %v %v", done, err)
	}
	if _, _, err := a.Add(&agentv1.BlobChunk{Sha256: sum[:], Offset: 5, Data: []byte("f")}); err == nil {
		t.Fatal("a chunk at the wrong offset was taken")
	}
	if b, done, err := a.Add(&agentv1.BlobChunk{Sha256: sum[:], Offset: 3, Data: []byte("def")}); err != nil || !done || string(b) != "abcdef" {
		t.Fatalf("whole: %q %v %v", b, done, err)
	}
	if a.Offset(sum[:]) != 0 {
		t.Fatal("a completed blob is still kept")
	}
	if _, _, err := a.Add(&agentv1.BlobChunk{Sha256: []byte("short")}); err == nil {
		t.Fatal("a hash that isn't a sha256 was taken")
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Second, 3: 8 * time.Second, 6: time.Minute, 20: time.Minute} {
		for range 100 {
			if d := Backoff(n); d < want*8/10 || d > want*12/10 {
				t.Fatalf("Backoff(%d) = %v, want %v ±20%%", n, d, want)
			}
		}
	}
}

// Keep dials again after a failed dial and after a dropped connection, until its context ends.
func TestKeepReconnects(t *testing.T) {
	backoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoff = Backoff })
	url := serve(t, func(c *Conn) { c.Close() }) // every connection drops at once
	ctx, cancel := context.WithCancel(t.Context())
	var dials, served atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Keep(ctx, func(ctx context.Context) (*Conn, error) {
			if dials.Add(1) <= 2 {
				return nil, errors.New("Control Plane unreachable")
			}
			return Dial(ctx, url, nil)
		}, func(ctx context.Context, c *Conn) error {
			served.Add(1)
			_, err := c.Recv(ctx)
			return err
		}, nil)
	}()
	for deadline := time.Now().Add(10 * time.Second); served.Load() < 3; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("dials %d, connections served %d", dials.Load(), served.Load())
		}
	}
	cancel()
	<-done
}

// A peer that stops answering pings ends the connection, instead of leaving a dead one open.
func TestPingEndsDeadConnection(t *testing.T) {
	ctx := t.Context()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	url := serve(t, func(c *Conn) { <-block }) // never reads: never answers a ping
	c, err := dial(ctx, url, nil, nil, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	go func() { c.Recv(ctx) }() // the client side reads, as an Agent does
	select {
	case <-c.Done():
		if !strings.Contains(c.Err().Error(), "ping") {
			t.Fatalf("ended with %v", c.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a dead peer kept the connection open")
	}
}

// Send on a closed connection says so, every time: never queued for a writer that is gone.
func TestSendOnClosed(t *testing.T) {
	url := serve(t, func(c *Conn) { c.Recv(t.Context()) })
	c, err := Dial(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for range 1000 {
		if err := c.Send(t.Context(), hello("late")); err == nil {
			t.Fatal("Send on a closed connection reported success")
		}
	}
}

// What a peer can get wrong: a plain HTTP request where a WebSocket is expected, a text frame, bytes that aren't an
// Envelope, a blob larger than the limit, a resume past a blob's end.
func TestPeerMistakes(t *testing.T) {
	var accepted atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := Accept(w, r)
		accepted.Store(fmt.Sprint(err))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := accepted.Load().(string); got == "<nil>" || got == "" {
		t.Fatalf("a plain request upgraded: %q", got)
	}

	ctx := t.Context()
	for name, frame := range map[string]func(*websocket.Conn) error{
		"text frame": func(ws *websocket.Conn) error { return ws.Write(ctx, websocket.MessageText, []byte("hi")) },
		"not an Envelope": func(ws *websocket.Conn) error {
			return ws.Write(ctx, websocket.MessageBinary, []byte{0xff, 0xff, 0xff})
		},
	} {
		got := make(chan error, 1)
		url := serve(t, func(c *Conn) { _, err := c.Recv(ctx); got <- err })
		ws, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := frame(ws); err != nil {
			t.Fatal(err)
		}
		if err := <-got; err == nil || !strings.HasPrefix(err.Error(), "wire:") {
			t.Errorf("%s: %v", name, err)
		}
		ws.CloseNow()
	}

	maxBlob = 8
	t.Cleanup(func() { maxBlob = 256 << 20 })
	var a Assembler
	sum := sha256.Sum256([]byte("more than eight bytes"))
	if _, _, err := a.Add(&agentv1.BlobChunk{Sha256: sum[:], Data: []byte("more than eight bytes")}); err == nil ||
		!strings.Contains(err.Error(), "larger than 8 bytes") {
		t.Errorf("too large: %v", err)
	}
	c := &Conn{}
	if err := c.SendBlob(ctx, sum[:], []byte("abc"), 4); err == nil || !strings.Contains(err.Error(), "past its 3 bytes") {
		t.Errorf("resume past the end: %v", err)
	}
}

// A connection that lasted long enough starts the backoff over: after it the next wait is the first one again.
func TestKeepStartsBackoffOver(t *testing.T) {
	var mu sync.Mutex
	var waits []int
	backoff = func(n int) time.Duration { mu.Lock(); waits = append(waits, n); mu.Unlock(); return time.Millisecond }
	stable = 0
	t.Cleanup(func() { backoff, stable = Backoff, time.Minute })
	url := serve(t, func(c *Conn) { time.Sleep(5 * time.Millisecond); c.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	dials := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		Keep(ctx, func(ctx context.Context) (*Conn, error) {
			if dials++; dials <= 2 {
				return nil, errors.New("Control Plane unreachable")
			}
			if dials == 4 {
				cancel()
			}
			return Dial(ctx, url, nil)
		}, func(ctx context.Context, c *Conn) error { _, err := c.Recv(ctx); return err }, nil)
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(waits) < 3 || waits[0] != 0 || waits[1] != 1 || waits[2] != 0 {
		t.Fatalf("waits %v, want 0 1 then 0 again after a connection that lasted", waits)
	}
}

// A send waiting for room in a full queue gives up when its context ends.
func TestSendGivesUp(t *testing.T) {
	c := &Conn{control: make(chan *agentv1.Envelope), done: make(chan struct{})} // no room, no writer
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Send(ctx, hello("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
