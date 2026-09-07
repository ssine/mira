package node

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func gbkSSHTextDecoder(input []byte) ([]byte, error) {
	return simplifiedchinese.GBK.NewDecoder().Bytes(input)
}

func TestSSHCommandTextWriterNormalizesMixedWindowsOutput(t *testing.T) {
	var output bytes.Buffer
	writer := newSSHCommandTextWriter(&output, gbkSSHTextDecoder)
	utf16Line := []byte{'W', 0, 'I', 0, 'N', 0, ':', 0, 0x2d, 0x4e, 0x87, 0x65, '\r', 0, '\n', 0}
	gbkLine := []byte{0xbb, 0xee, 0xb6, 0xaf, '\r', '\n'}
	utf8Line := []byte("PowerShell:\u4e2d\u6587\r\n")
	for _, chunk := range [][]byte{utf16Line[:11], utf16Line[11:], gbkLine[:1], gbkLine[1:], utf8Line} {
		if _, err := writer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, expected := output.String(), "WIN:\u4e2d\u6587\r\n\u6d3b\u52a8\r\nPowerShell:\u4e2d\u6587\r\n"; actual != expected {
		t.Fatalf("normalized output %q, expected %q", actual, expected)
	}
}

func TestSSHCommandTextWriterFlushesTailsAndBoundsUnbrokenLines(t *testing.T) {
	var output bytes.Buffer
	writer := newSSHCommandTextWriter(&output, gbkSSHTextDecoder)
	tail := []byte{'O', 0, 'K', 0}
	if _, err := writer.Write(tail); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "OK" {
		t.Fatalf("UTF-16 tail was not normalized: %x", output.Bytes())
	}

	output.Reset()
	writer = newSSHCommandTextWriter(&output, func([]byte) ([]byte, error) {
		return nil, errors.New("must not decode an overlong raw line")
	})
	raw := bytes.Repeat([]byte{0xff}, sshTextLineLimit+1)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("\nnext\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), append(append(raw, '\n'), []byte("next\n")...)) {
		t.Fatal("overlong line was changed instead of passing through")
	}
}

func TestSSHCommandTextWriterFallsBackToRawBytesOnDecoderError(t *testing.T) {
	var output bytes.Buffer
	writer := newSSHCommandTextWriter(&output, func([]byte) ([]byte, error) {
		return nil, errors.New("invalid legacy text")
	})
	raw := []byte{0xff, 0xfe, '\r', '\n'}
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), raw) {
		t.Fatalf("decoder error changed bytes: %x", output.Bytes())
	}
}
