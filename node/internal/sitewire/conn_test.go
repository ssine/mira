package sitewire

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/transport"
)

type testFrame struct {
	kind int
	data []byte
}
type testRaw struct {
	transport.Conn
	in, out chan testFrame
	done    chan struct{}
	once    sync.Once
}

func (r *testRaw) ReadMessage() (int, []byte, error) {
	select {
	case f := <-r.in:
		return f.kind, f.data, nil
	case <-r.done:
		return 0, nil, net.ErrClosed
	}
}
func (r *testRaw) WriteMessage(k int, p []byte) error {
	select {
	case r.out <- testFrame{k, bytes.Clone(p)}:
		return nil
	case <-r.done:
		return net.ErrClosed
	}
}
func (r *testRaw) SetReadLimit(int64)   {}
func (r *testRaw) Close() error         { r.once.Do(func() { close(r.done) }); return nil }
func (r *testRaw) LocalAddr() net.Addr  { return nil }
func (r *testRaw) RemoteAddr() net.Addr { return nil }
func pair() (*Conn, *Conn) {
	a, b := make(chan testFrame), make(chan testFrame)
	done := make(chan struct{})
	ra := &testRaw{in: a, out: b, done: done}
	rb := &testRaw{in: b, out: a, done: make(chan struct{})}
	return New(ra), New(rb)
}
func TestBidirectionalHalfCloseAndLargeBody(t *testing.T) {
	a, b := pair()
	defer a.Close()
	defer b.Close()
	data := bytes.Repeat([]byte("abcdef"), 1024*1024)
	done := make(chan error, 1)
	go func() {
		_, err := a.Write(data)
		if err == nil {
			err = a.CloseWrite()
		}
		done <- err
	}()
	got, err := io.ReadAll(b)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("large stream: %d %v", len(got), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() { b.Write([]byte("response after EOF")); b.CloseWrite() }()
	got, err = io.ReadAll(a)
	if err != nil || string(got) != "response after EOF" {
		t.Fatalf("half close: %q %v", got, err)
	}
}
func TestReadDeadlineCanBeChangedAndCleared(t *testing.T) {
	a, b := pair()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() { _, err := a.Read(make([]byte, 8)); done <- err }()
	a.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if err := <-done; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
	a.SetReadDeadline(time.Time{})
	go b.Write([]byte("ok"))
	p := make([]byte, 2)
	if _, err := io.ReadFull(a, p); err != nil || string(p) != "ok" {
		t.Fatal(err)
	}
}
func TestWriteDeadlineInterruptsBlockedWrite(t *testing.T) {
	r := &testRaw{in: make(chan testFrame), out: make(chan testFrame), done: make(chan struct{})}
	a := New(r)
	defer a.Close()
	done := make(chan error, 1)
	go func() { _, err := a.Write([]byte("blocked")); done <- err }()
	a.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not unblock")
	}
}
func TestMalformedFrames(t *testing.T) {
	for _, f := range []testFrame{{websocket.TextMessage, []byte("bad")}, {websocket.BinaryMessage, []byte{dataFrame}}, {websocket.BinaryMessage, []byte{finFrame, 0}}, {websocket.BinaryMessage, []byte{99}}} {
		r := &testRaw{in: make(chan testFrame, 1), out: make(chan testFrame), done: make(chan struct{})}
		r.in <- f
		a := New(r)
		_, err := a.Read(make([]byte, 4))
		a.Close()
		if err == nil {
			t.Fatal("accepted malformed frame")
		}
	}
}
func TestCloseUnblocksBackpressure(t *testing.T) {
	a, b := pair()
	defer b.Close()
	done := make(chan error, 1)
	go func() { _, err := a.Write(bytes.Repeat([]byte("x"), FrameBytes*8)); done <- err }()
	time.Sleep(20 * time.Millisecond)
	a.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected closed writer")
		}
	case <-time.After(time.Second):
		t.Fatal("writer leaked")
	}
}
