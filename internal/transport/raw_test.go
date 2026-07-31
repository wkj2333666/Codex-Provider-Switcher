package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wkj2333666/Codex-Provider-Switcher/internal/config"
)

func TestRunWithRawMaskedRFC6455Frames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	upstreamMessages := make(chan receivedMessage, 4)
	closeUpstream := make(chan struct{})
	upstreamCloseDone := make(chan error, 1)
	socket := startWebSocketServer(t, func(connection *websocket.Conn) {
		for range 4 {
			messageType, payload, err := connection.Read(ctx)
			upstreamMessages <- receivedMessage{messageType: messageType, payload: payload, err: err}
			if err != nil {
				return
			}
		}
		<-closeUpstream
		upstreamCloseDone <- connection.Close(websocket.StatusGoingAway, "server restart")
	})

	clientStream, switcherStream := net.Pipe()
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- Run(ctx, Options{
			Config: config.Config{Provider: "provider-a", Socket: socket},
			Stdin:  switcherStream,
			Stdout: switcherStream,
		})
	}()

	request := "GET /rpc?raw=1 HTTP/1.1\r\n" +
		"Host: desktop.test\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n\r\n"
	if _, err := io.WriteString(clientStream, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(clientStream)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("ReadResponse() error = %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	if extension := response.Header.Get("Sec-WebSocket-Extensions"); extension != "" {
		t.Fatalf("downstream extension = %q, want empty", extension)
	}
	_ = response.Body.Close()

	for index, size := range []int{125, 126, 65536} {
		message := jsonMessageOfLength(t, size)
		writeRawClientFrame(t, clientStream, true, rawOpcodeText, message, [4]byte{1, 2, 3, byte(index + 4)})
		upstream := <-upstreamMessages
		if upstream.err != nil || upstream.messageType != websocket.MessageText || !bytes.Equal(upstream.payload, message) {
			t.Fatalf("upstream %d-byte message = %v bytes, type %v, error %v", size, len(upstream.payload), upstream.messageType, upstream.err)
		}
	}

	fragments := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"thread/`),
		[]byte(`start","params":{"cwd":"/tmp",`),
		[]byte(`"modelProvider":"wrong"}}`),
	}
	writeRawClientFrame(t, clientStream, false, rawOpcodeText, fragments[0], [4]byte{9, 8, 7, 6})
	writeRawClientFrame(t, clientStream, false, rawOpcodeContinuation, fragments[1], [4]byte{5, 4, 3, 2})
	writeRawClientFrame(t, clientStream, true, rawOpcodeContinuation, fragments[2], [4]byte{1, 3, 5, 7})
	fragmented := <-upstreamMessages
	if fragmented.err != nil || !bytes.Contains(fragmented.payload, []byte(`"modelProvider":"provider-a"`)) || !bytes.Contains(fragmented.payload, []byte(`"cwd":"/tmp"`)) {
		t.Fatalf("fragmented upstream payload = %s, error %v", fragmented.payload, fragmented.err)
	}

	writeRawClientFrame(t, clientStream, true, rawOpcodePing, []byte("probe"), [4]byte{4, 3, 2, 1})
	pong := readRawServerFrame(t, reader)
	if !pong.fin || pong.opcode != rawOpcodePong || pong.masked || string(pong.payload) != "probe" {
		t.Fatalf("pong = %#v", pong)
	}

	close(closeUpstream)
	closeFrame := readRawServerFrame(t, reader)
	wantClosePayload := append([]byte{0x03, 0xe9}, []byte("server restart")...)
	if !closeFrame.fin || closeFrame.opcode != rawOpcodeClose || closeFrame.masked || !bytes.Equal(closeFrame.payload, wantClosePayload) {
		t.Fatalf("close frame = %#v, want payload %x", closeFrame, wantClosePayload)
	}
	writeRawClientFrame(t, clientStream, true, rawOpcodeClose, closeFrame.payload, [4]byte{8, 6, 4, 2})

	if err := <-upstreamCloseDone; err != nil {
		t.Fatalf("upstream Close() error = %v", err)
	}
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not stop after raw close handshake")
	}
}

const (
	rawOpcodeContinuation byte = 0x0
	rawOpcodeText         byte = 0x1
	rawOpcodeClose        byte = 0x8
	rawOpcodePing         byte = 0x9
	rawOpcodePong         byte = 0xa
)

type rawFrame struct {
	fin     bool
	opcode  byte
	masked  bool
	payload []byte
}

func writeRawClientFrame(t *testing.T, writer io.Writer, fin bool, opcode byte, payload []byte, maskingKey [4]byte) {
	t.Helper()
	first := opcode
	if fin {
		first |= 0x80
	}
	header := []byte{first}
	switch length := len(payload); {
	case length <= 125:
		header = append(header, 0x80|byte(length))
	case length <= 65535:
		header = append(header, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(length))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:], uint64(length))
	}
	header = append(header, maskingKey[:]...)
	masked := append([]byte(nil), payload...)
	for index := range masked {
		masked[index] ^= maskingKey[index%len(maskingKey)]
	}
	writeRawBytes(t, writer, header)
	writeRawBytes(t, writer, masked)
}

func readRawServerFrame(t *testing.T, reader io.Reader) rawFrame {
	t.Helper()
	base := make([]byte, 2)
	if _, err := io.ReadFull(reader, base); err != nil {
		t.Fatal(err)
	}
	frame := rawFrame{fin: base[0]&0x80 != 0, opcode: base[0] & 0x0f, masked: base[1]&0x80 != 0}
	length := uint64(base[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			t.Fatal(err)
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(reader, extended); err != nil {
			t.Fatal(err)
		}
		length = binary.BigEndian.Uint64(extended)
	}
	if length > 64<<20 {
		t.Fatalf("raw frame length %d exceeds test limit", length)
	}
	var maskingKey [4]byte
	if frame.masked {
		if _, err := io.ReadFull(reader, maskingKey[:]); err != nil {
			t.Fatal(err)
		}
	}
	frame.payload = make([]byte, int(length))
	if _, err := io.ReadFull(reader, frame.payload); err != nil {
		t.Fatal(err)
	}
	if frame.masked {
		for index := range frame.payload {
			frame.payload[index] ^= maskingKey[index%len(maskingKey)]
		}
	}
	return frame
}

func writeRawBytes(t *testing.T, writer io.Writer, data []byte) {
	t.Helper()
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			t.Fatal(err)
		}
		if written == 0 {
			t.Fatal(fmt.Errorf("raw frame write: %w", io.ErrShortWrite))
		}
		data = data[written:]
	}
}
