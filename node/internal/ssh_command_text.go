package node

import (
	"bytes"
	"encoding/binary"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

const sshTextLineLimit = 64 * 1024

type sshLegacyDecoder func([]byte) ([]byte, error)

// sshCommandTextWriter normalizes one Windows command-output line at a time.
// cmd.exe /u writes its own messages as UTF-16LE, while child programs may
// write UTF-8 or the machine's OEM code page to the same pipe. Long unbroken
// or explicitly raw streams pass through instead of being retained without a
// bound or guessed to be text.
type sshCommandTextWriter struct {
	destination io.Writer
	decodeOEM   sshLegacyDecoder
	pending     []byte
	rawLine     bool
	err         error
}

func newSSHCommandTextWriter(destination io.Writer, decodeOEM sshLegacyDecoder) *sshCommandTextWriter {
	return &sshCommandTextWriter{destination: destination, decodeOEM: decodeOEM}
}

func (writer *sshCommandTextWriter) Write(input []byte) (int, error) {
	consumed := len(input)
	for len(input) > 0 && writer.err == nil {
		if writer.rawLine {
			end := bytes.IndexByte(input, '\n')
			if end < 0 {
				writer.err = writeSSHTextBytes(writer.destination, input)
				break
			}
			writer.err = writeSSHTextBytes(writer.destination, input[:end+1])
			writer.rawLine = false
			input = input[end+1:]
			continue
		}

		writer.pending = append(writer.pending, input...)
		input = nil
		for writer.err == nil {
			end, utf16Line, complete := sshTextLineEnd(writer.pending, false)
			if !complete {
				break
			}
			writer.err = writer.flush(writer.pending[:end], utf16Line)
			writer.pending = writer.pending[end:]
		}
		if writer.err == nil && len(writer.pending) > sshTextLineLimit {
			writer.err = writeSSHTextBytes(writer.destination, writer.pending)
			writer.pending = writer.pending[:0]
			writer.rawLine = true
		}
	}
	return consumed, writer.err
}

func (writer *sshCommandTextWriter) Close() error {
	if writer.err != nil {
		return writer.err
	}
	if len(writer.pending) == 0 {
		return nil
	}
	_, utf16Line, _ := sshTextLineEnd(writer.pending, true)
	writer.err = writer.flush(writer.pending, utf16Line)
	writer.pending = nil
	return writer.err
}

func sshTextLineEnd(input []byte, final bool) (int, bool, bool) {
	for offset := 0; offset < len(input); {
		index := bytes.IndexByte(input[offset:], '\n')
		if index < 0 {
			break
		}
		index += offset
		if index >= 2 && input[index-2] == '\r' && input[index-1] == 0 {
			if index+1 >= len(input) {
				if !final {
					return 0, false, false
				}
			} else if input[index+1] == 0 {
				return index + 2, true, true
			}
		}
		return index + 1, false, true
	}
	return len(input), final && looksLikeUTF16LE(input), final
}

func looksLikeUTF16LE(input []byte) bool {
	if len(input) < 2 || len(input)%2 != 0 {
		return false
	}
	zeros := 0
	for index := 1; index < len(input); index += 2 {
		if input[index] == 0 {
			zeros++
		}
	}
	return zeros > 0 && zeros*4 >= len(input)
}

func (writer *sshCommandTextWriter) flush(input []byte, utf16Line bool) error {
	output := input
	if utf16Line && len(input)%2 == 0 {
		units := make([]uint16, len(input)/2)
		for index := range units {
			units[index] = binary.LittleEndian.Uint16(input[index*2:])
		}
		output = []byte(string(utf16.Decode(units)))
	} else if !utf8.Valid(input) && writer.decodeOEM != nil {
		decoded, err := writer.decodeOEM(input)
		if err == nil && utf8.Valid(decoded) && !bytes.Contains(decoded, []byte("\ufffd")) {
			output = decoded
		}
	}
	return writeSSHTextBytes(writer.destination, output)
}

func writeSSHTextBytes(destination io.Writer, input []byte) error {
	for len(input) > 0 {
		written, err := destination.Write(input)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		input = input[written:]
	}
	return nil
}
