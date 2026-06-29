//go:build !tinygo

package main

import (
	"net"
	"testing"
	"time"
)

func TestLineTransportReadsAndWritesJSONLines(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	left := newLineTransport(a)
	defer left.Close()

	go func() {
		_, _ = b.Write([]byte("{\"type\":\"hello\"}\n"))
	}()
	line, err := left.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if string(line) != `{"type":"hello"}` {
		t.Fatalf("line = %q", string(line))
	}

	read := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := b.Read(buf)
		read <- string(buf[:n])
	}()
	if err := left.WriteLine([]byte(`{"type":"pong"}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	select {
	case got := <-read:
		if got != `{"type":"pong"}`+"\n" {
			t.Fatalf("write = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for write")
	}
}
