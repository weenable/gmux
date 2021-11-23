package gmux

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAcceptBacklog = 1024
)

var (
	ErrGoAway          = errors.New("stream id overflows, should start a new connection")
	ErrTimeout = errors.New("timeout")
)

type writeRequest struct {
	prio uint32
	frame Frame
	result chan writeResult
}

type writeResult struct {
	n int
	err error
}

type Session struct {
	conn io.ReadWriteCloser

	config *Config
	nextStreamID uint32
	nextStreamIDLock sync.Mutex

	bucket int32
	bucketNotify chan struct{}

	streams	map[uint32] *Stream
	streamLock sync.Mutex

	chAccepts chan *Stream

	die chan struct{}
	dieOnce sync.Once

	socketWriteError atomic.Value
	socketReadError atomic.Value
	chSocketWriteError chan struct{}
	chSocketReadError chan struct{}

	protoError atomic.Value
	chProtoError chan struct{}

	dataReady int32

	goAway int32

	deadline atomic.Value

	shaper chan writeRequest
	writes chan writeRequest
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketNotify = make(chan struct{}, 1)
	s.shaper = make(chan writeRequest)
	s.writes = make(chan writeRequest)
	s.chSocketReadError = make(chan struct{})
	s.chSocketWriteError = make(chan struct{})
	s.chProtoError = make(chan struct{})

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}

	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	return s
}

func (s *Session) IsClosed() bool {
	select {
	case <- s.die:
		return true
	default:
		return false
	}
}

func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}

	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid % 2 {
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}
	s.nextStreamIDLock.Unlock()

	stream := 
}

func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case stream := <- s.chAccepts:
		return stream, nil
	case <- deadline:
		return nil, ErrTimeout
	case <- s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <- s.chProtoError:
		return nil, s.protoError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	}
}

func (s *Session) Close() error {
	var once bool
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		return s.conn.Close()
	} else {
		return io.ErrClosedPipe
	}
}

func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:

	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <- tickerPing.C:
			s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), tickerPing.C, 0)
			s.notifyBucket()
		case <- tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				if atomic.LoadInt32(&s.bucket) > 0 {
					s.Close()
					return
				}
			}
		case <-s.die:
			return
		}
	}
}

func (s *Session) writeFrameInternal(f Frame, deadline  <- chan time.Time, prio uint32) (int, error) {
	req := writeRequest{
		prio: prio,
		frame: f,
		result: make(chan writeResult, 1),
	}

	select {
	case s.shaper <- req:
	case <- s.die:
		return 0, io.ErrClosedPipe
	case <- s.chSocketWriteError:
		return 0, s.socketWriteError.Load().(error)
	case <- deadline:
		return 0, ErrTimeout
	}

	select {
	case result := <- req.result:
		return result.n, result.err
	case <- s.die:
		return 0, io.ErrClosedPipe
	case <- s.chSocketWriteError:
		return 0, s.socketWriteError.Load().(error)
	case <- deadline:
		return 0, ErrTimeout
	}
}