package gmux

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type Stream struct {
	id   uint32
	sess *Session

	buffers [][]byte
	heads [][]byte

	bufferLock sync.Mutex
	frameSize int

	chReadEvent chan struct{}

	die     chan struct{}
	dieOnce sync.Once

	chFinEvent chan struct{}
	finEventOnce sync.Once

	readDeadLine atomic.Value
	writeDeadLine atomic.Value

	numWritten uint32

	peerConsumed uint32
	peerWindow uint32
	chUpdate chan struct{}
}

func (s *Stream) pushBytes(buf []byte) (written int, err error) {
	s.bufferLock.Lock()
	defer s.bufferLock.Unlock()
	s.buffers = append(s.buffers, buf)
	s.heads = append(s.heads, buf)
	return
}

func (s *Stream) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:

	}
}

func (s *Stream) update(consumed uint32, window uint32) {
	atomic.StoreUint32(&s.peerConsumed, consumed)
	atomic.StoreUint32(&s.peerWindow, window)
	select {
	case s.chUpdate <- struct{}{}:
	default:

	}
}

func (s *Stream) fin() {
	s.finEventOnce.Do(func() {
		close(s.chFinEvent)
	})
}

func (s *Stream) Write(b []byte) (n int, err error) {
	if s.sess.config.Version == 2 {

	}

	var deadline <-chan time.Time
	if d, ok := s.writeDeadLine.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:

	}

	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)
	bts := b
	for len(bts) > 0 {
		sz := len(bts)
		if sz > s.frameSize {
			sz = s.frameSize
		}

		frame.data = bts[:sz]
		bts = bts[sz:]
		n, err := s.sess.writeFrameInternal(frame, deadline, s.numWritten)
		s.numWritten++
		sent += n
		if err != nil {
			return sent, err
		}
	}

	return sent, nil
}

func (s *Stream) waitRead() error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadLine.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-s.chReadEvent:
		return nil
	case <-s.chFinEvent:
		s.bufferLock.Lock()
		defer s.bufferLock.Unlock()
		if len(s.buffers) > 0 {
			return nil
		}
		return io.EOF
	case <-s.sess.chSocketReadError:
		return s.sess.socketReadError.Load().(error)
	case <-s.sess.chSocketWriteError:
		return s.sess.socketWriteError.Load().(error)
	case <-deadline:
		return ErrTimeout
	case <-s.die:
		return io.ErrClosedPipe
	}
}

func (s *Stream) Read(b []byte) (n int, err error) {
	for {
		n, err = s.tryRead(b)
		if err == ErrWouldBlock {
			if ew := s.waitRead(); ew != nil {
				return 0, ew
			}
		} else {
			return n, err
		}
	}
}

func (s *Stream) tryRead(b []byte) (n int, err error) {
	if s.sess.config.Version == 2 {
		return s.tryReadv2(b)
	}

	if len(b) == 0 {
		return 0, nil
	}

	s.bufferLock.Lock()
	if len(s.buffers) > 0 {
		n = copy(b, s.buffers[0])
		s.buffers[0] = s.buffers[0][n:]
		if len(s.buffers[0]) == 0 {
			s.buffers[0] = nil
			s.buffers = s.buffers[1:]

			defaultAllocator.Put(s.heads[0])
			s.heads = s.heads[1:]
		}
	}
	s.bufferLock.Unlock()
	if n > 0 {
		s.sess.returnTokens(n)
		return n, nil
	}

	select {
	case <- s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

func (s *Stream) tryReadv2(b []byte) (n int, err error) {
	return 0, nil
}

func (s *Stream) sessionClose() {
	s.dieOnce.Do(func() {
		close(s.die)
	})
}

func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.sess = sess
	s.chReadEvent = make(chan struct{}, 1)
	s.chUpdate = make(chan  struct{}, 1)
	s.frameSize = frameSize
	s.die = make(chan struct{})
	s.chFinEvent = make(chan  struct{})
	s.peerWindow = initialPeerWindow
	return s
}

func (s *Stream) Close() error {
	var once bool
	var err error
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		_, err = s.sess.writeFrame(newFrame(byte(s.sess.config.Version), cmdFIN, s.id))
		s.sess.streamClosed(s.id)
		return err
	} else {
		return io.ErrClosedPipe
	}
}

func (s *Stream) recycleTokens() (n int) {
	s.bufferLock.Lock()
	for k := range s.buffers {
		n += len(s.buffers[k])
		defaultAllocator.Put(s.heads[k])
	}
	s.buffers = nil
	s.heads = nil
	s.bufferLock.Unlock()
	return
}
