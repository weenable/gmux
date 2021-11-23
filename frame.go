package gmux

const (
	cmdSYN byte = iota
	cmdNOP
)

type Frame struct {
	ver  byte
	cmd  byte
	sid  uint32
	data []byte
}

func newFrame(version byte, cmd byte, sid uint32) Frame {
	return Frame{ver: version, cmd: cmd, sid: sid}
}
