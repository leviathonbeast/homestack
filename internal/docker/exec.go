package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ExecSession is an interactive shell inside a container.
type ExecSession struct {
	ID   string
	conn net.Conn
	r    *bufio.Reader
}

func (e *ExecSession) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *ExecSession) Write(p []byte) (int, error) { return e.conn.Write(p) }
func (e *ExecSession) Close() error                { return e.conn.Close() }

// Exec starts cmd in the container with a TTY and returns the raw
// bidirectional stream (this is what `docker exec -it` does).
func (c *Client) Exec(ctx context.Context, containerID string, cmd []string, user string, cols, rows int) (*ExecSession, error) {
	create := map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          true,
		"Cmd":          cmd,
		"Env":          []string{"TERM=xterm-256color"},
	}
	if user != "" {
		create["User"] = user
	}
	if cols > 0 && rows > 0 {
		create["ConsoleSize"] = []int{rows, cols}
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.getJSON(ctx, "POST", "/containers/"+url.PathEscape(containerID)+"/exec", nil, create, &created); err != nil {
		return nil, err
	}

	// Starting an attached exec "hijacks" the HTTP connection into a raw
	// stream, so this request is written by hand.
	var d net.Dialer
	conn, err := d.DialContext(ctx, c.network, c.address)
	if err != nil {
		return nil, fmt.Errorf("can't reach Docker: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true})
	req := fmt.Sprintf("POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		created.ID, len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, fmt.Errorf("exec failed: %s", strings.TrimSpace(string(msg)))
	}
	return &ExecSession{ID: created.ID, conn: conn, r: br}, nil
}

// ExecResize tells the shell its terminal size changed.
func (c *Client) ExecResize(ctx context.Context, execID string, cols, rows int) error {
	q := url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}
	return c.getJSON(ctx, "POST", "/exec/"+url.PathEscape(execID)+"/resize", q, nil, nil)
}

// ExecExitCode returns the exit code once the exec has finished.
func (c *Client) ExecExitCode(ctx context.Context, execID string) (int, bool) {
	var v struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := c.getJSON(ctx, "GET", "/exec/"+url.PathEscape(execID)+"/json", nil, nil, &v); err != nil {
		return 0, false
	}
	return v.ExitCode, !v.Running
}
