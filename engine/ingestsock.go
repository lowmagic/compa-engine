package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/internal/store"
)

// Bringing evidence into a case whose store a chat holds: the store's lock
// refuses a second process, so `compa in` run beside the chat, from the
// chat's own shell or another terminal, sends what it reads to the chat,
// which ingests it into its store as the CLI would.

// ErrCaseHeld is the error for a case whose store another process holds,
// or another open in this one.
var ErrCaseHeld = store.ErrLocked

// ingestPointer is the file in a store's directory naming the socket of
// the session holding it, there only while it serves.
const ingestPointer = "ingest"

// maxIngestHeader bounds the header a client sends before its content.
const maxIngestHeader = 64 << 10

// ingestHeader is what a client sends before its content, on one line.
type ingestHeader struct {
	Location string `json:"location"`
	Note     string `json:"note"`
}

// ingestReply is what the session answers, on one line.
type ingestReply struct {
	Item  IngestedItem `json:"item"`
	Error string       `json:"error,omitempty"`
}

// IngestedItem is evidence brought in through a session serving its
// store: the item's ID, where it came from and the engineer's note, its
// size, whether it is new, and its name in the conversation.
type IngestedItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Note     string `json:"note,omitempty"`
	Bytes    int64  `json:"bytes"`
	Lines    int64  `json:"lines"`
	New      bool   `json:"new"`
}

// IngestServer serves the session's store to `compa in` over a Unix
// socket only its owner can reach: the socket in a directory of its own
// that no one else can enter, its path named in the store's directory,
// itself readable by no one else. Close stops it.
type IngestServer struct {
	s       *Session
	ln      net.Listener
	dir     string
	pointer string
	wg      sync.WaitGroup

	// mu guards taken, the items brought in not yet taken, and conns,
	// the clients being answered, which Close ends.
	mu    sync.Mutex
	taken []IngestedItem
	conns map[net.Conn]bool
}

// ServeIngest starts serving the session's store to `compa in`; ctx bounds
// only the start.
func (s *Session) ServeIngest(ctx context.Context) (*IngestServer, error) {
	// The socket's own directory is short, as a socket's path must be,
	// and owner-only, whatever the umask.
	dir, err := os.MkdirTemp("", "compa-")
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ingest: %w", err)
	}
	path := filepath.Join(dir, "ingest.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ingest: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ingest: %w", err)
	}
	srv := &IngestServer{s: s, ln: ln, dir: dir, pointer: filepath.Join(s.Store().Dir, ingestPointer), conns: map[net.Conn]bool{}}
	if err := os.WriteFile(srv.pointer, []byte(path+"\n"), 0o600); err != nil {
		ln.Close()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("ingest: %w", err)
	}
	srv.wg.Add(1)
	go srv.serve()
	return srv, nil
}

// Socket is the socket's path, for a shell's COMPA_INGEST.
func (srv *IngestServer) Socket() string { return srv.ln.Addr().String() }

// Take returns the items brought in since it was last called, oldest
// first.
func (srv *IngestServer) Take() []IngestedItem {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	out := srv.taken
	srv.taken = nil
	return out
}

// Close stops serving, ends the clients being answered and waits for
// them, and removes the socket and the store's pointer to it.
func (srv *IngestServer) Close() error {
	err := srv.ln.Close()
	srv.mu.Lock()
	for c := range srv.conns {
		c.Close()
	}
	srv.mu.Unlock()
	srv.wg.Wait()
	if rerr := os.Remove(srv.pointer); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && err == nil {
		err = rerr
	}
	if rerr := os.RemoveAll(srv.dir); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

func (srv *IngestServer) serve() {
	defer srv.wg.Done()
	for {
		conn, err := srv.ln.Accept()
		if err != nil {
			return
		}
		srv.mu.Lock()
		srv.conns[conn] = true
		srv.mu.Unlock()
		srv.wg.Add(1)
		go func() {
			defer srv.wg.Done()
			defer func() {
				srv.mu.Lock()
				delete(srv.conns, conn)
				srv.mu.Unlock()
				conn.Close()
			}()
			srv.answer(conn)
		}()
	}
}

// answer ingests one client's content and says what came of it.
func (srv *IngestServer) answer(conn net.Conn) {
	r := bufio.NewReader(io.LimitReader(conn, maxIngestHeader))
	line, err := r.ReadBytes('\n')
	var h ingestHeader
	if err == nil {
		err = json.Unmarshal(line, &h)
	}
	var reply ingestReply
	if err != nil || h.Location == "" {
		reply.Error = "the request was not one compa in sends"
	} else {
		// What the header line's reader read ahead belongs to the content.
		content := io.MultiReader(buffered(r), conn)
		reply.Item, err = srv.ingest(content, h)
		if err != nil {
			reply.Error = err.Error()
		}
	}
	_ = json.NewEncoder(conn).Encode(reply)
}

// buffered is what r has read ahead of what it gave.
func buffered(r *bufio.Reader) io.Reader {
	b, _ := r.Peek(r.Buffered())
	return strings.NewReader(string(b))
}

func (srv *IngestServer) ingest(content io.Reader, h ingestHeader) (IngestedItem, error) {
	it, isNew, err := srv.s.Ingest(content, h.Location, h.Note)
	if err != nil {
		return IngestedItem{}, err
	}
	name, err := srv.s.NameEvidence(it.Ref.ID)
	if err != nil {
		return IngestedItem{}, fmt.Errorf("kept as %s but not named: %w", it.Ref.ID, err)
	}
	got := IngestedItem{ID: it.Ref.ID, Name: name, Location: it.Location, Note: it.Note, Bytes: it.Size.Bytes, Lines: it.Size.Lines, New: isNew}
	srv.mu.Lock()
	srv.taken = append(srv.taken, got)
	srv.mu.Unlock()
	return got, nil
}

// SendIngest sends what r yields to the session serving socket, to keep
// as evidence from location with the engineer's note, and returns what it
// kept.
func SendIngest(ctx context.Context, socket string, r io.Reader, location, note string) (IngestedItem, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return IngestedItem{}, fmt.Errorf("the chat holding the case did not answer: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(conn).Encode(ingestHeader{Location: location, Note: note}); err != nil {
		return IngestedItem{}, err
	}
	if _, err := io.Copy(conn, r); err != nil {
		return IngestedItem{}, err
	}
	// The content ends where the client stops writing; the answer comes
	// back on the same connection.
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return IngestedItem{}, fmt.Errorf("%s is not a Unix socket", socket)
	}
	if err := uc.CloseWrite(); err != nil {
		return IngestedItem{}, err
	}
	var reply ingestReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return IngestedItem{}, fmt.Errorf("the chat holding the case gave no answer: %w", err)
	}
	if reply.Error != "" {
		return IngestedItem{}, errors.New(reply.Error)
	}
	return reply.Item, nil
}

// IngestSocket is the socket of the session holding a case's store, as it
// names it in the store, or "" where none serves it.
func (e *Engine) IngestSocket(ctx context.Context, id string) (string, error) {
	root, err := e.artifacts.Root(ctx, id)
	if errors.Is(err, ErrNoArtifacts) {
		root, err = "", nil
	}
	if err != nil {
		return "", err
	}
	dir, _, err := e.storeLocation(root, id)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, ingestPointer))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
