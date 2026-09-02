// The Python worker: starting it, framing requests onto its stdin, and
// demultiplexing the event stream it writes back. Requests run concurrently,
// so every event carries the id of the request it belongs to.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// ---- worker ---------------------------------------------------------------

type Worker struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	vocab [][]byte
	// Largest prompt this machine can hold, measured by the worker against the
	// weights it actually loaded. Published so clients can plan, enforced so
	// they do not have to.
	maxContext int

	// Requests run concurrently, so events arrive interleaved and tagged with
	// the request they belong to. One goroutine reads them and posts each to
	// the handler waiting for it.
	mu     sync.Mutex
	nextID uint32
	routes map[uint32]chan event
	dead   error
	died   chan struct{} // closed once, when the worker stops

	writeMu sync.Mutex // one whole line at a time
}

// submit registers a request and hands it to the worker, returning the channel
// its events will arrive on. Callers must release the id when they are done,
// finished or not, or the routing table grows forever.
func (w *Worker) submit(payload map[string]any) (uint32, chan event, error) {
	w.mu.Lock()
	if w.dead != nil {
		err := w.dead
		w.mu.Unlock()
		return 0, nil, err
	}
	id := w.nextID
	w.nextID++
	// Buffered generously: the pump must not stall behind one slow HTTP client
	// while other requests are still generating. If the buffer does fill, the
	// pump treats the client as gone rather than waiting for it (see pump).
	ch := make(chan event, 1024)
	w.routes[id] = ch
	w.mu.Unlock()

	payload["id"] = id
	b, err := json.Marshal(payload)
	if err != nil {
		w.release(id)
		return 0, nil, err
	}
	if err := w.writeLine(b); err != nil {
		w.release(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (w *Worker) writeLine(b []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_, err := w.in.Write(append(b, '\n'))
	return err
}

func (w *Worker) release(id uint32) {
	w.mu.Lock()
	delete(w.routes, id)
	w.mu.Unlock()
}

// cancel tells the worker to drop a request. A sequence that nobody is
// reading still shares every decode step with the others, so an abandoned one
// slows everyone down until its max_tokens run out.
func (w *Worker) cancel(id uint32) {
	b, _ := json.Marshal(map[string]any{"cancel": id})
	_ = w.writeLine(b)
}

func (w *Worker) deadErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

// pump reads the worker's event stream and routes each event to its request.
// It is the only reader of w.out.
func (w *Worker) pump() {
	for {
		k, err := w.out.ReadByte()
		if err == nil {
			var b [8]byte
			_, err = io.ReadFull(w.out, b[:])
			if err == nil {
				id := binary.LittleEndian.Uint32(b[:4])
				ev := event{k, binary.LittleEndian.Uint32(b[4:])}
				w.mu.Lock()
				ch := w.routes[id]
				w.mu.Unlock()
				if ch == nil {
					continue
				}
				select {
				case ch <- ev:
				default:
					// The handler has not drained a thousand events: its client
					// is not reading. Blocking here would freeze every other
					// request, so this one is dropped instead.
					w.release(id)
					close(ch)
					w.cancel(id)
				}
				continue
			}
		}
		// The worker died or the pipe closed. Everyone waiting has to be told,
		// or they wait forever.
		w.mu.Lock()
		w.dead = fmt.Errorf("worker stopped: %w", err)
		for _, ch := range w.routes {
			close(ch)
		}
		w.routes = map[uint32]chan event{}
		w.mu.Unlock()
		close(w.died)
		return
	}
}

func NewWorker(python, script, model string, entries int) (*Worker, error) {
	vf, err := os.CreateTemp("", "vocab-*.bin")
	if err != nil {
		return nil, err
	}
	vf.Close()
	defer os.Remove(vf.Name())

	cmd := exec.Command(python, script, model, vf.Name(), strconv.Itoa(entries))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	br := bufio.NewReaderSize(stdout, 1<<16)
	log.Println("loading model in worker...")
	b, err := br.ReadByte() // wait for 'R'
	if err != nil || b != 'R' {
		return nil, fmt.Errorf("worker failed to start (got %q, err %v)", b, err)
	}
	var ctxBuf [4]byte
	if _, err := io.ReadFull(br, ctxBuf[:]); err != nil {
		return nil, fmt.Errorf("worker did not report a context ceiling: %w", err)
	}
	maxContext := int(binary.LittleEndian.Uint32(ctxBuf[:]))

	vocab, err := loadVocab(vf.Name())
	if err != nil {
		return nil, err
	}
	log.Printf("worker ready, vocab %d entries, context ceiling %d tokens",
		len(vocab), maxContext)
	w := &Worker{cmd: cmd, in: stdin, out: br, vocab: vocab,
		maxContext: maxContext, routes: map[uint32]chan event{},
		died: make(chan struct{})}
	go w.pump()
	return w, nil
}

func loadVocab(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(raw[:4])
	v := make([][]byte, n)
	off := 4
	for i := uint32(0); i < n; i++ {
		l := int(binary.LittleEndian.Uint16(raw[off : off+2]))
		off += 2
		v[i] = raw[off : off+l]
		off += l
	}
	return v, nil
}

// event is one message from the worker. Kinds: 'X' prompt over the ceiling,
// '!' request could not be rendered, 'C' cache reuse, 'K' thinking state,
// 'P' prefill done, 'T' token, 'F' truncated flag, 'E' end.
type event struct {
	kind byte
	val  uint32
}

// errClosed means the request's channel was closed under the handler: either
// the worker died or the pump gave up on this client.
var errClosed = errors.New("request channel closed")

// recv waits for the next event of a request.
func recv(ch chan event) (event, error) {
	ev, ok := <-ch
	if !ok {
		return event{}, errClosed
	}
	return ev, nil
}
