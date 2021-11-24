package gmux

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
)

func setupServer(tb testing.TB) (addr string, stopfunc func(), client net.Conn, err error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", nil, nil, err
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConnection(conn)
	}()

	addr = ln.Addr().String()
	conn , err := net.Dial("tcp", addr)
	if err != nil {
		ln.Close()
		return "", nil, nil, err
	}

	return ln.Addr().String(), func(){ln.Close()}, conn, nil
}


func handleConnection(conn net.Conn) {
	session, _ := Server(conn, nil)
	for {
		if stream, err := session.AcceptStream(); err == nil {
			go func(s io.ReadWriteCloser) {
				buf := make([]byte, 65536)
				for {
					n, err := s.Read(buf)
					if err != nil {
						return
					}
					s.Write(buf[:n])
				}
			}(stream)
		} else {
			return
		}
	}
}

func setupServerV2(tb *testing.TB) (addr string, stopfunc func(), client net.Conn, err error) {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", nil, nil, err
	}

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConnectionV2(conn)
	}()

	addr = ln.Addr().String()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		ln.Close()
		return "", nil, nil, err
	}

	return ln.Addr().String(), func() {ln.Close()}, conn, nil
}

func handleConnectionV2(conn net.Conn) {
	config := DefaultConfig()
	config.Version = 2
	session, _ := Server(conn, config)
	for {
		if stream, err := session.AcceptStream(); err != nil {
			go func(s io.ReadWriteCloser) {
				buf := make([]byte, 65536)
				for {
					n, err := s.Read(buf)
					if err != nil {
						return
					}
					s.Write(buf[:n])
				}
			}(stream)
		} else {
			return
		}
	}
}

func TestEcho(t *testing.T) {
	_, stop, cli, err := setupServer(t)
	if err != nil {
		t.Fatal(err)
	}

	defer stop()
	session, _ := Client(cli, nil)

	stream, _ := session.OpenStream()
	const N = 100
	buf := make([]byte, 10)
	var sent string
	var received string
	for i := 0; i < N; i++ {
		msg := fmt.Sprintf("hello%v", i)
		stream.Write([]byte(msg))
		sent += msg
		if n, err := stream.Read(buf); err != nil {
			t.Fatal(err)
		} else {
			received += string(buf[:n])
		}
	}

	if sent != received {
		t.Fatal("data mismatch")
	}

	session.Close()
}

func TestParallel(t *testing.T) {
	_, stop, cli, err := setupServer(t)
	if err != nil {
		t.Fatal(err)
	}

	defer stop()
	session, _ := Client(cli, nil)

	par := 1000
	messages := 100
	var wg sync.WaitGroup
	wg.Add(par)
	for i := 0; i < par; i++ {
		stream , _ := session.OpenStream()
		go func(s * Stream) {
			buf := make([]byte, 20)
			for j := 0; j < messages; j++ {
				msg := fmt.Sprintf("hello%v", j)
				s.Write([]byte(msg))
				if _, err := s.Read(buf); err != nil {
					break
				}
			}
			s.Close()
			wg.Done()
		}(stream)
	}

	t.Log("created ", session.NumStreams(), " streams")
	wg.Wait()
	session.Close()
}