package gmux

import (
	"container/heap"
	"encoding/binary"
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
	ErrWouldBlock = errors.New("operation would block on IO")
	ErrInvalidProtocol = errors.New("invalid protocol")
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

type buffersWriter interface {
	WriteBuffers(v [][]byte) (n int, err error)
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
	socketReadErrorOnce sync.Once
	socketWriteErrorOnce sync.Once

	protoError atomic.Value
	chProtoError chan struct{}
	protoErrorOnce sync.Once


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

	go s.shaperLoop()
	go s.recvLoop()
	go s.sendLoop()

	if !config.KeepAliveDisabled {
		go s.keepalive()
	}
	return s
}

func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

func (s *Session) notifyWriteError(err error) {
	s.socketWriteErrorOnce.Do(func() {
		s.socketWriteError.Store(err)
		close(s.chSocketWriteError)
	})
}

func (s *Session) notifyReadError(err error) {
	s.socketReadErrorOnce.Do(func() {
		s.socketReadError.Store(err)
		close(s.chSocketReadError)
	})
}

func (s *Session) notifyProtoError(err error) {
	s.protoErrorOnce.Do(func() {
		s.protoError.Store(err)
		close(s.chProtoError)
	})
}


func (s *Session) sendLoop() {
	var buf []byte
	var n int
	var err error
	var vec [][]byte

	bw, ok := s.conn.(buffersWriter)
	if ok {
		buf = make([]byte, headerSize)
		vec = make([][]byte, 2)
	} else {
		buf = make([]byte, (1<<16)+headerSize)
	}

	for {
		select {
		case <-s.die:
			return
		case request := <-s.writes:
			buf[0] = request.frame.ver
			buf[1] = request.frame.cmd
			binary.LittleEndian.PutUint16(buf[2:], uint16(len(request.frame.data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)

			if len(vec) > 0 {
				vec[0] = buf[:headerSize]
				vec[1] = request.frame.data
				n, err = bw.WriteBuffers(vec)
			} else {
				copy(buf[headerSize:], request.frame.data)
				n, err = s.conn.Write(buf[:headerSize+len(request.frame.data)])
			}

			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n: n,
				err: err,
			}

			request.result <- result
			close(request.result)
			if err != nil {
				s.notifyWriteError(err)
				return
			}
		}
	}
}

func (s *Session) recvLoop() {
	var hdr rawHeader
	var updHdr updHeader

	for {
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			select {
			case <-s.bucketNotify:
			case <-s.die:
				return
			}
		}

		if _, err := io.ReadFull(s.conn, hdr[:]); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)
			if hdr.Version() != byte(s.config.Version) {
				s.notifyProtoError(ErrInvalidProtocol)
				return
			}
			sid := hdr.StreamID()
			switch hdr.Cmd() {
			case cmdNOP:
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[sid]; !ok {
					stream := newStream(sid, s.config.MaxFrameSize, s)
					s.streams[sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				s.streamLock.Lock()
				if stream, ok := s.streams[sid]; ok {
					stream.fin()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				if hdr.Length() > 0 {
					newbuf := defaultAllocator.Get(int(hdr.Length()))
					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						s.streamLock.Lock()
						if stream, ok := s.streams[sid]; ok {
							stream.pushBytes(newbuf)
							atomic.AddInt32(&s.bucket, -int32(written))
							stream.notifyReadEvent()
						}
						s.streamLock.Unlock()
					} else {
						s.notifyReadError(err)
						return
					}
				}
			case cmdUPD:
				if _, err := io.ReadFull(s.conn, updHdr[:]); err == nil {
					s.streamLock.Lock()
					if stream, ok := s.streams[sid]; ok {
						stream.update(updHdr.Consumed(), updHdr.Window())
					}
					s.streamLock.Unlock()
				} else {
					s.notifyReadError(err)
					return
				}
			default:

			}
		} else {
			s.notifyReadError(err)
			return
		}
	}
}

func (s *Session) shaperLoop() {
	var reqs shaperHeap
	var next writeRequest
	var chWrite chan writeRequest

	for {
		if len(reqs) > 0 {
			chWrite = s.writes
			next = heap.Pop(&reqs).(writeRequest)
		} else {
			chWrite = nil
		}

		select {
		case <-s.die:
			return
		case r := <-s.shaper:
			if chWrite != nil {
				heap.Push(&reqs, next)
			}
			heap.Push(&reqs, r)
		case chWrite <- next:
		}
	}
}

func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

func (s *Session) IsClosed() bool {
	select {
	case <- s.die:
		return true
	default:
		return false
	}
}

func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 {
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	delete(s.streams, sid)
	s.streamLock.Unlock()
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

	stream := newStream(sid, s.config.MaxFrameSize, s)

	if _, err := s.writeFrame(newFrame(byte(s.config.Version), cmdSYN, sid)); err != nil {
		return nil, err
	}

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	select {
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		return nil, s.socketWriteError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	default:
		s.streams[sid] = stream
		return stream, nil
	}
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

func (s *Session) writeFrame(f Frame) (n int, err error) {
	return s.writeFrameInternal(f, nil, 0)
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