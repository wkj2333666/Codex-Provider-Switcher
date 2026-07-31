package transport

import (
	"io"
	"net"
	"sync"
	"time"
)

type streamConn struct {
	reader    io.Reader
	writer    io.Writer
	closed    chan struct{}
	closeOnce sync.Once
}

func newStreamConn(reader io.Reader, writer io.Writer) *streamConn {
	return &streamConn{reader: reader, writer: writer, closed: make(chan struct{})}
}

func (connection *streamConn) Read(data []byte) (int, error) {
	select {
	case <-connection.closed:
		return 0, net.ErrClosed
	default:
		return connection.reader.Read(data)
	}
}

func (connection *streamConn) Write(data []byte) (int, error) {
	written := 0
	for written < len(data) {
		select {
		case <-connection.closed:
			return written, net.ErrClosed
		default:
		}
		count, err := connection.writer.Write(data[written:])
		written += count
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (connection *streamConn) Close() error {
	var closeErr error
	connection.closeOnce.Do(func() {
		close(connection.closed)
		if closer, ok := connection.reader.(io.Closer); ok {
			closeErr = closer.Close()
		}
	})
	return closeErr
}

func (*streamConn) LocalAddr() net.Addr              { return streamAddr("stdio-local") }
func (*streamConn) RemoteAddr() net.Addr             { return streamAddr("stdio-remote") }
func (*streamConn) SetDeadline(time.Time) error      { return nil }
func (*streamConn) SetReadDeadline(time.Time) error  { return nil }
func (*streamConn) SetWriteDeadline(time.Time) error { return nil }

type streamAddr string

func (address streamAddr) Network() string { return "stdio" }
func (address streamAddr) String() string  { return string(address) }

type singleConnListener struct {
	connection net.Conn
	closed     chan struct{}
	closeOnce  sync.Once
	acceptOnce sync.Once
	accepted   bool
}

func newSingleConnListener(connection net.Conn) *singleConnListener {
	return &singleConnListener{connection: connection, closed: make(chan struct{})}
}

func (listener *singleConnListener) Accept() (net.Conn, error) {
	listener.acceptOnce.Do(func() { listener.accepted = true })
	if listener.accepted {
		listener.accepted = false
		return listener.connection, nil
	}
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *singleConnListener) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return nil
}

func (*singleConnListener) Addr() net.Addr { return streamAddr("stdio-listener") }
