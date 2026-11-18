package collect

import (
	"bytes"
	"errors"
)

var errOutputFull = errors.New("output cap reached")

// cappedBuffer keeps at most limit bytes and records whether anything was
// dropped. Writes past the limit report errOutputFull so producers can stop.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCapped(limit int) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return 0, errOutputFull
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return room, errOutputFull
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) line(s string) error {
	if _, err := c.Write([]byte(s)); err != nil {
		return err
	}
	_, err := c.Write([]byte{'\n'})
	return err
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// commandWriter caps a command's output like cappedBuffer, but never fails
// a write: the command keeps running and its excess output is discarded,
// so a full cap cannot make it die of a broken pipe.
type commandWriter struct{ c *cappedBuffer }

func (w commandWriter) Write(p []byte) (int, error) {
	w.c.Write(p)
	return len(p), nil
}
