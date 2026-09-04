package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

var ErrEventTooLarge = errors.New("sse event exceeded limit")

type Event struct {
	Name string
	Data []byte
}

type Decoder struct {
	reader       *bufio.Reader
	maxEventSize int
}

func NewDecoder(reader io.Reader, maxEventSize int) *Decoder {
	return &Decoder{
		reader:       bufio.NewReader(reader),
		maxEventSize: maxEventSize,
	}
}

func (d *Decoder) Next() (Event, error) {
	var name string
	var data bytes.Buffer
	eventBytes := 0
	sawField := false
	sawData := false

	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if sawData {
					return Event{Name: name, Data: data.Bytes()}, nil
				}
				return Event{}, io.EOF
			}
			return Event{}, err
		}
		eventBytes += len(line)
		if d.maxEventSize > 0 && eventBytes > d.maxEventSize {
			return Event{}, ErrEventTooLarge
		}

		line = trimLineEnding(line)
		if len(line) == 0 {
			if !sawField {
				continue
			}
			if !sawData {
				name = ""
				sawField = false
				continue
			}
			return Event{Name: name, Data: data.Bytes()}, nil
		}
		if line[0] == ':' {
			continue
		}

		field, value, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			field = line
			value = nil
		} else if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			name = string(value)
			sawField = true
		case "data":
			if sawData {
				_ = data.WriteByte('\n')
			}
			sawData = true
			_, _ = data.Write(value)
			sawField = true
		default:
			sawField = true
		}
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var line []byte
	for {
		part, err := d.reader.ReadSlice('\n')
		line = append(line, part...)
		if d.maxEventSize > 0 && len(line) > d.maxEventSize {
			return nil, ErrEventTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		return line, nil
	}
}

func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	return line
}
