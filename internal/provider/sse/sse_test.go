package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDecoderParsesEvents(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(": keepalive\r\nevent: delta\r\ndata: hello\r\ndata: world\r\n\r\n"), 1024)

	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if event.Name != "delta" || string(event.Data) != "hello\nworld" {
		t.Fatalf("event = %+v", event)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second next err = %v, want EOF", err)
	}
}

func TestDecoderSkipsCommentOnlyEvents(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(": ping\n\ndata: payload\n\n"), 1024)

	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(event.Data) != "payload" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestDecoderSkipsDataLessEventAtEOF(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("event: ping"), 1024)

	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
}

func TestDecoderDispatchesEmptyDataEvent(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("event: empty\ndata:\n\n"), 1024)

	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if event.Name != "empty" || string(event.Data) != "" {
		t.Fatalf("event = %+v", event)
	}
}

func TestDecoderPreservesMultilineEmptyData(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data:\ndata:\ndata: tail\n\n"), 1024)

	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(event.Data) != "\n\ntail" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestDecoderDispatchesPartialEventAtEOF(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data: payload"), 1024)

	event, err := decoder.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(event.Data) != "payload" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestDecoderRejectsOversizedEvent(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data: 123456\n\n"), 8)

	_, err := decoder.Next()
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("err = %v, want ErrEventTooLarge", err)
	}
}
