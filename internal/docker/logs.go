package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/url"
	"strings"
)

// LogLine is one line of container output.
type LogLine struct {
	Stream int    `json:"s"` // 1 = stdout, 2 = stderr
	Time   string `json:"t"` // RFC3339Nano from Docker
	Text   string `json:"l"`
}

// FollowLogs streams a container's logs line by line until ctx ends or the
// container stops. tail is how many past lines to include ("all" or a number);
// since (a unix timestamp, optional) skips older lines.
func (c *Client) FollowLogs(ctx context.Context, id string, tail, since string, follow bool, fn func(LogLine)) error {
	tty, err := c.ContainerTTY(ctx, id)
	if err != nil {
		return err
	}
	q := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"timestamps": {"1"},
		"tail":       {tail},
	}
	if follow {
		q.Set("follow", "1")
	}
	if since != "" {
		q.Set("since", since)
	}
	body, err := c.Stream(ctx, "GET", "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer body.Close()

	emit := func(stream int, raw string) {
		raw = strings.TrimRight(raw, "\r")
		ts, text, ok := strings.Cut(raw, " ")
		if !ok {
			ts, text = "", raw
		}
		fn(LogLine{Stream: stream, Time: ts, Text: text})
	}

	if tty {
		// TTY containers send a plain byte stream.
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			emit(1, sc.Text())
		}
		return sc.Err()
	}

	// Non-TTY containers multiplex stdout and stderr: each frame has an
	// 8-byte header [stream, 0, 0, 0, size(4 bytes big-endian)].
	r := bufio.NewReaderSize(body, 32<<10)
	partial := map[int]*bytes.Buffer{1: {}, 2: {}}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		stream := int(hdr[0])
		if stream != 2 {
			stream = 1
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		buf := partial[stream]
		if _, err := io.CopyN(buf, r, int64(size)); err != nil {
			return err
		}
		for {
			line, err := buf.ReadString('\n')
			if err != nil { // no newline yet: keep the fragment for the next frame
				rest := line
				buf.Reset()
				buf.WriteString(rest)
				break
			}
			emit(stream, strings.TrimSuffix(line, "\n"))
		}
		if buf.Len() > 256<<10 { // guard against a runaway line
			emit(stream, buf.String())
			buf.Reset()
		}
	}
}
