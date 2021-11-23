package gmux

import "sync"

type Stream struct {
	id   uint32
	sess *Session

	frameSize int

	chReadEvent chan struct{}

	die     chan struct{}
	dieOnce sync.Once

	chUpdate chan struct{}
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

}
