// SPDX-FileCopyrightText: (C) 2024 Intel Corporation
// SPDX-License-Identifier: Apache 2.0

package serviceinfo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fido-device-onboard/go-fdo/cbor"
)

// ErrSizeTooSmall indicates that a chunk could not be read due to insufficient
// max size.
var ErrSizeTooSmall = errors.New("not enough size for chunk")

type pipeReader interface {
	io.ReadCloser
	CloseWithError(error) error
}

type pipeWriter interface {
	io.WriteCloser
	CloseWithError(error) error
}

func bufferedPipe() (pipeReader, pipeWriter) {
	pipe := &bufPipe{
		ch: make(chan struct{}, 1),
	}
	return pipe, pipe
}

type bufPipe struct {
	sync.Mutex
	buf bytes.Buffer

	err error
	ch  chan struct{}
}

func (b *bufPipe) Read(p []byte) (int, error) {
	for {
		b.Lock()
		if b.buf.Len() > 0 {
			n, _ := b.buf.Read(p)
			b.Unlock()
			return n, nil
		}
		b.Unlock()
		_, ok := <-b.ch
		if !ok {
			return 0, b.err
		}
	}
}

func (b *bufPipe) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()

	if b.err != nil {
		return 0, io.ErrClosedPipe
	}

	select {
	case b.ch <- struct{}{}:
	default:
	}
	return b.buf.Write(p)
}

func (b *bufPipe) Close() error { return b.CloseWithError(io.EOF) }

func (b *bufPipe) CloseWithError(err error) error {
	b.Lock()
	defer b.Unlock()

	if b.err == nil {
		b.err = err
		close(b.ch)
	}
	return nil
}

// ChunkReader reads ServiceInfo chunked at some MTU.
type ChunkReader struct {
	readers <-chan pipeReader
	r       pipeReader
	rkey    cbor.RawBytes
	key     string
	buffer  []byte
}

// ReadChunk reads ServiceInfo chunked at some MTU. The values contain any
// number of logical ServiceInfos. When no more ServiceInfo will be available,
// an io.EOF error is returned.
func (r *ChunkReader) ReadChunk(size uint16) (*KV, error) {
	if r.r == nil {
		// Get the next reader, which will be chunked into zero or more KVs
		nextReader, open := <-r.readers
		if !open {
			return nil, io.EOF
		}
		r.r = nextReader

		// Limit the max bytes read for the key to size minus 7 (min overhead,
		// see note below)
		keyReader := io.LimitReader(r.r, int64(size-7))

		// Read key as raw CBOR
		if err := cbor.NewDecoder(keyReader).Decode(&r.rkey); err != nil {
			_ = r.r.CloseWithError(err)
			r.r = nil
			if errors.Is(err, io.EOF) {
				err = ErrSizeTooSmall
			}
			return nil, fmt.Errorf("could not read service info key: %w", err)
		}

		// Read key into a string
		if err := cbor.Unmarshal([]byte(r.rkey), &r.key); err != nil {
			_ = r.r.CloseWithError(err)
			r.r = nil
			return nil, fmt.Errorf("could not decode service info key: %w", err)
		}
	}

	// Subtract overhead of ServiceInfo CBOR
	//
	// 	- 1 for first byte of ServiceInfo: 0x82
	// 	- length of marshaled ServiceInfo key (at least 4 bytes)
	// 	- 1 for first byte of marshaled ServiceInfo value
	//  - 0-2 bytes based on the length of value (unknown at this point)
	//  - value 1 or more bytes
	//
	// The size of the value that will be read is unknown, but its max is, so a
	// max overhead can be calculated.
	maxOverhead := 1 + len(r.rkey) + 1
	if int(size)-maxOverhead >= 24 {
		maxOverhead++
	}
	if int(size)-maxOverhead >= 256 {
		maxOverhead++
	}
	if int(size)-maxOverhead <= 0 {
		return nil, ErrSizeTooSmall
	}

	// Grow buffer if not large enough
	if len(r.buffer) < int(size)-maxOverhead {
		r.buffer = make([]byte, int(size)-maxOverhead)
	}

	// Read data, ensuring ServiceInfo will not be larger than size once
	// marshaled to CBOR
	n, err := io.ReadFull(r.r, r.buffer[:int(size)-maxOverhead])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		r.r = nil
		if n == 0 {
			return r.ReadChunk(size)
		}
	} else if err != nil {
		_ = r.r.CloseWithError(err)
		return nil, err
	}

	// Copy read bytes from the buffer into a new byte slice for the KV
	val := make([]byte, n)
	copy(val, r.buffer[:n])

	return &KV{
		Key: r.key,
		Val: val,
	}, nil
}

// Close the reader if no more reads will be performed so that the Writer
// errors rather than deadlocks.
func (r *ChunkReader) Close() error {
	if r.r == nil {
		return nil
	}
	return r.r.Close()
}

// ChunkWriter expects ServiceInfo key-values to be written in the correct
// order and appropriate MTU. When all chunks have been written, it must be
// closed exactly once with either Close or CloseWithError.
type ChunkWriter struct {
	readers chan<- pipeReader
	w       pipeWriter
	prevKey string
	pipe    func() (pipeReader, pipeWriter)
}

// WriteChunk is called with chunked ServiceInfos.
func (w *ChunkWriter) WriteChunk(kv *KV) error {
	if kv.Key != w.prevKey {
		if w.w != nil {
			if err := w.w.Close(); err != nil {
				return err
			}
		}

		pr, pw := w.pipe()
		w.readers <- pr
		w.w = pw
		w.prevKey = kv.Key

		if err := cbor.NewEncoder(w.w).Encode(kv.Key); err != nil {
			return err
		}
	}

	_, err := w.w.Write(kv.Val)
	return err
}

// Close must be called exactly once when all ServiceInfo have been written.
func (w *ChunkWriter) Close() (err error) {
	close(w.readers)
	if w.w == nil {
		return nil
	}
	return w.w.Close()
}

// CloseWithError causes reads from the associated UnchunkReader to error with
// the given error.
func (w *ChunkWriter) CloseWithError(err error) error {
	close(w.readers)
	if w.w == nil {
		return nil
	}
	return w.w.CloseWithError(err)
}

// UnchunkReader gets a new io.Reader for each logical ServiceInfo.
type UnchunkReader struct {
	readers <-chan pipeReader
}

// NextServiceInfo gets a new io.Reader for a logical ServiceInfo. The reader
// contains the unchunked value of the ServiceInfo so that the consumer does
// not need to be aware of the MTU.
func (r *UnchunkReader) NextServiceInfo() (key string, val io.ReadCloser, ok bool) {
	val, ok = <-r.readers
	if !ok {
		return "", nil, false
	}
	if err := cbor.NewDecoder(val).Decode(&key); err != nil {
		return "", nil, false
	}
	return key, val, true
}

// UnchunkWriter is used for writing unchunked logical ServiceInfos. Each
// ServiceInfo is written by first calling NextServiceInfo to give it a module
// and message name, then the full body is written via zero or more calls to
// Write.
type UnchunkWriter struct {
	readers chan<- pipeReader
	w       pipeWriter
	closed  bool
	pipe    func() (pipeReader, pipeWriter)
}

// NextServiceInfo must be called once before each logical ServiceInfo.
func (w *UnchunkWriter) NextServiceInfo(moduleName, messageName string) error {
	if err := w.nextPipe(false); err != nil {
		return err
	}
	return cbor.NewEncoder(w.w).Encode(moduleName + ":" + messageName)
}

// ForceNewMessage causes the next (*ChunkReader).ReadChunk to return
// ErrSizeTooSmall. This in turn forces the next KV to be put into a new
// ServiceInfo message.
//
// This method facilitates implementing custom chunking conventions, provided
// that the MTU used for automatic chunking at the client level is known to the
// implementer.
func (w *UnchunkWriter) ForceNewMessage() error {
	return w.nextPipe(true)
}

func (w *UnchunkWriter) nextPipe(forceNewMessage bool) error {
	if w.closed {
		return io.ErrClosedPipe
	}
	if w.w != nil {
		_ = w.w.Close()
	}
	pr, pw := w.pipe()
	w.readers <- pr
	if forceNewMessage {
		_ = pw.Close()
	}
	w.w = pw
	return nil
}

// Write may be called any number of times to write the contents of a
// ServiceInfo value, but it must be preceded by a call to Next and must be
// succeeded by a call to either Next or Close.
func (w *UnchunkWriter) Write(p []byte) (n int, err error) { return w.w.Write(p) }

// Close is called when all ServiceInfos have been written and no further calls
// to Next or Write will be made.
func (w *UnchunkWriter) Close() error {
	if w.closed {
		return io.ErrClosedPipe
	}
	w.closed = true
	close(w.readers)

	if w.w == nil {
		_, w.w = io.Pipe()
	}
	return w.w.Close()
}

// CloseWithError causes reads from the associated ChunkReader to error with
// the given error.
func (w *UnchunkWriter) CloseWithError(err error) error {
	if w.closed {
		return io.ErrClosedPipe
	}
	w.closed = true

	if w.w == nil {
		pr, pw := io.Pipe()
		w.readers <- pr
		w.w = pw
	}

	close(w.readers)
	return w.w.CloseWithError(err)
}

// NewChunkInPipe creates a ChunkWriter and UnchunkReader pair. All chunks sent
// to the writer will be unchunked and emitted from the reader.
func NewChunkInPipe(buffers int) (*UnchunkReader, *ChunkWriter) {
	readers := make(chan pipeReader)
	pipe := func() (pipeReader, pipeWriter) { return io.Pipe() }
	if buffers > 0 {
		readers = make(chan pipeReader, buffers)
		pipe = bufferedPipe
	}
	return &UnchunkReader{readers: readers}, &ChunkWriter{readers: readers, pipe: pipe}
}

// NewChunkOutPipe creates a ChunkReader and UnchunkWriter pair. All service
// info sent to the writer will be chunked using the given MTU and emitted from
// the reader.
func NewChunkOutPipe(buffers int) (*ChunkReader, *UnchunkWriter) {
	readers := make(chan pipeReader)
	pipe := func() (pipeReader, pipeWriter) { return io.Pipe() }
	if buffers > 0 {
		readers = make(chan pipeReader, buffers)
		pipe = bufferedPipe
	}
	return &ChunkReader{readers: readers}, &UnchunkWriter{readers: readers, pipe: pipe}
}
