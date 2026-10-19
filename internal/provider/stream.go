package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Generous, since reasoning models can think for minutes in silence.
	defaultStreamIdleTimeout = 5 * time.Minute

	// Bigger events pass through unparsed and the cost is estimated.
	maxEventBytes = 1 << 20
)

// meter reads token usage out of one provider's stream events.
type meter interface {
	// observe reads one event's data and reports whether to hide it.
	observe(data []byte) (hide bool)

	// usage returns the counts so far and whether the final count arrived.
	usage() (u Usage, final bool)

	textBytes() int
}

type requestInfo struct {
	model      string
	inputBytes int
	maxTokens  int
}

func readRequestInfo(body []byte) requestInfo {
	var v struct {
		Model               string `json:"model"`
		MaxTokens           int    `json:"max_tokens"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &v)

	info := requestInfo{model: v.Model, inputBytes: len(body), maxTokens: v.MaxTokens}
	if v.MaxCompletionTokens > 0 {
		info.maxTokens = v.MaxCompletionTokens
	}
	return info
}

// Stream passes events through unchanged while metering usage. Always Close it.
type Stream struct {
	body   io.ReadCloser
	r      *bufio.Reader
	cancel context.CancelFunc
	meter  meter
	info   requestInfo

	idle      time.Duration
	idleTimer *time.Timer
	idleFired atomic.Bool

	// raw means an event was too big, so bytes now pass through unparsed.
	raw      bool
	rawBytes int
	err      error

	onFinish func(u Usage, estimated bool)
	once     sync.Once
	done     chan struct{}
}

func newStream(body io.ReadCloser, cancel context.CancelFunc, m meter, info requestInfo, idle time.Duration) *Stream {
	s := &Stream{
		body:   body,
		r:      bufio.NewReaderSize(body, 64<<10),
		cancel: cancel,
		meter:  m,
		info:   info,
		idle:   idle,
		done:   make(chan struct{}),
	}
	s.idleTimer = time.AfterFunc(idle, func() {
		s.idleFired.Store(true)
		cancel()
	})
	return s
}

// OnFinish sets where Close reports the cost. Set it before handing the stream on.
func (s *Stream) OnFinish(f func(u Usage, estimated bool)) { s.onFinish = f }

func (s *Stream) Done() <-chan struct{} { return s.done }

// Next returns the next piece to forward, or io.EOF when the provider has finished.
func (s *Stream) Next() ([]byte, error) {
	for {
		if s.err != nil {
			return nil, s.err
		}

		var chunk []byte
		var err error
		if s.raw {
			chunk, err = s.readRaw()
		} else {
			chunk, err = s.readEvent()
			if !s.raw && len(chunk) > 0 && s.observe(chunk) {
				chunk = nil
			}
		}

		if err != nil {
			s.err = s.explain(err)
		}
		if len(chunk) > 0 {
			return chunk, nil
		}
	}
}

// Close stops the upstream call and reports the cost once.
func (s *Stream) Close() error {
	s.once.Do(func() {
		defer close(s.done)
		s.idleTimer.Stop()
		s.cancel()
		s.body.Close()

		u, estimated := s.usage()
		if s.onFinish != nil {
			s.onFinish(u, estimated)
		}
	})
	return nil
}

// readEvent reads up to and including the blank line that ends an event.
func (s *Stream) readEvent() ([]byte, error) {
	var ev []byte
	lineStart := true
	for {
		piece, err := s.r.ReadSlice('\n')
		if len(piece) > 0 {
			s.idleTimer.Reset(s.idle)
		}
		blank := lineStart && (string(piece) == "\n" || string(piece) == "\r\n")
		ev = append(ev, piece...)
		lineStart = len(piece) > 0 && piece[len(piece)-1] == '\n'

		if len(ev) > maxEventBytes {
			s.raw = true
			s.rawBytes += len(ev)
			return ev, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil || blank {
			return ev, err
		}
	}
}

func (s *Stream) readRaw() ([]byte, error) {
	buf := make([]byte, 32<<10)
	n, err := s.r.Read(buf)
	if n > 0 {
		s.idleTimer.Reset(s.idle)
		s.rawBytes += n
	}
	return buf[:n], err
}

func (s *Stream) observe(ev []byte) bool {
	var data [][]byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	if len(data) == 0 {
		return false
	}
	return s.meter.observe(bytes.Join(data, []byte("\n")))
}

func (s *Stream) explain(err error) error {
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if s.idleFired.Load() {
		return fmt.Errorf("no data from the provider for %s", s.idle)
	}
	return err
}

// usage is the provider's count if it arrived, otherwise an estimate of one token per byte.
func (s *Stream) usage() (Usage, bool) {
	u, final := s.meter.usage()
	if final {
		return u, false
	}

	if u.Model == "" {
		u.Model = s.info.model
	}
	if u.TotalInputTokens() == 0 {
		u.InputTokens = s.info.inputBytes
	}
	out := s.meter.textBytes() + s.rawBytes
	if s.info.maxTokens > 0 && out > s.info.maxTokens {
		out = s.info.maxTokens
	}
	if out > u.OutputTokens {
		u.OutputTokens = out
	}
	return u, true
}
