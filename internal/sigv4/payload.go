package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

var ErrPayloadHashMismatch = errors.New("request payload hash does not match x-amz-content-sha256")

// VerifiedBody hashes bytes as a handler streams them and reports a mismatch
// at EOF. A consumer must read to EOF before committing object metadata.
type VerifiedBody struct {
	reader     io.ReadCloser
	hasher     io.Writer
	expected   []byte
	actualHash interface{ Sum([]byte) []byte }
	finished   bool
	terminal   error
}

func NewVerifiedBody(reader io.ReadCloser, expectedHex string) (*VerifiedBody, error) {
	expected, err := hex.DecodeString(expectedHex)
	if err != nil || len(expected) != sha256.Size {
		return nil, errors.New("invalid expected SHA-256 payload hash")
	}
	h := sha256.New()
	return &VerifiedBody{reader: reader, hasher: h, actualHash: h, expected: expected}, nil
}

func (r *VerifiedBody) Read(buffer []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	n, readErr := r.reader.Read(buffer)
	if n > 0 {
		_, _ = r.hasher.Write(buffer[:n])
	}
	if readErr == io.EOF {
		if subtleCompare(r.actualHash.Sum(nil), r.expected) != 1 {
			r.terminal = ErrPayloadHashMismatch
			if n > 0 {
				return n, r.terminal
			}
			return 0, r.terminal
		}
		r.finished = true
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	if readErr != nil {
		r.terminal = readErr
	}
	return n, readErr
}

func (r *VerifiedBody) Close() error { return r.reader.Close() }

func (r *VerifiedBody) Verified() bool { return r.finished }

func subtleCompare(actual, expected []byte) int {
	if len(actual) != len(expected) {
		return 0
	}
	var difference byte
	for i := range actual {
		difference |= actual[i] ^ expected[i]
	}
	if difference == 0 {
		return 1
	}
	return 0
}
