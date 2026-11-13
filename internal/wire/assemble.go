package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sync"

	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// maxBlob bounds one blob being put together, so a broken or hostile peer can't fill the memory.
var maxBlob = 256 << 20 // tests use less

// Assembler puts blobs together from their chunks, which arrive in order. What arrived of a blob before its
// connection dropped is kept, and Offset says where to resume on the next one.
// ponytail: partial blobs in memory; spool them to the cache dir if blobs of hundreds of MB become usual.
type Assembler struct {
	mu    sync.Mutex
	parts map[string]*part
}

type part struct {
	buf bytes.Buffer
	sum hash.Hash
}

// Add takes a chunk; it returns the blob once complete, that is once what arrived hashes to the blob's sha256.
func (a *Assembler) Add(c *agentv1.BlobChunk) (blob []byte, complete bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := hex.EncodeToString(c.Sha256)
	if len(c.Sha256) != sha256.Size {
		return nil, false, fmt.Errorf("wire: a chunk of blob %q: not a sha256", key)
	}
	if a.parts == nil {
		a.parts = map[string]*part{}
	}
	p := a.parts[key]
	if p == nil {
		p = &part{sum: sha256.New()}
		a.parts[key] = p
	}
	if c.Offset != uint64(p.buf.Len()) {
		return nil, false, fmt.Errorf("wire: blob %s: a chunk at %d, want %d", key, c.Offset, p.buf.Len())
	}
	if p.buf.Len()+len(c.Data) > maxBlob {
		delete(a.parts, key)
		return nil, false, fmt.Errorf("wire: blob %s: larger than %d bytes", key, maxBlob)
	}
	p.buf.Write(c.Data)
	p.sum.Write(c.Data)
	if !bytes.Equal(p.sum.Sum(nil), c.Sha256) {
		return nil, false, nil
	}
	delete(a.parts, key)
	return p.buf.Bytes(), true, nil
}

// Offset is how much of a blob has arrived: where a BlobRequest resumes it.
func (a *Assembler) Offset(sha256 []byte) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.parts[hex.EncodeToString(sha256)]; p != nil {
		return uint64(p.buf.Len())
	}
	return 0
}
