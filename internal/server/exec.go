package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// containerExec opens a shell in a container over a WebSocket.
//
// Binary messages carry terminal bytes in both directions. Text messages from
// the browser are JSON control messages, e.g. {"type":"resize","cols":80,"rows":24}.
func (s *Server) containerExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	shell := q.Get("shell")
	user := q.Get("user")
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))

	var cmd []string
	switch shell {
	case "", "auto":
		// Prefer bash, fall back to sh (many images only have sh).
		cmd = []string{"/bin/sh", "-c", "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"}
	default:
		cmd = strings.Fields(shell)
	}

	// Same-origin check is on by default in websocket.Accept.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess, err := s.docker.Exec(ctx, id, cmd, user, cols, rows)
	if err != nil {
		_ = conn.Write(ctx, websocket.MessageBinary, []byte("\r\n\x1b[31m"+sentence(err.Error())+"\x1b[0m\r\n"))
		conn.Close(websocket.StatusNormalClosure, "exec failed")
		return
	}
	defer sess.Close()

	// Container -> browser
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Browser -> container
	go func() {
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var msg struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 {
					_ = s.docker.ExecResize(ctx, sess.ID, msg.Cols, msg.Rows)
				}
				continue
			}
			if _, err := sess.Write(data); err != nil {
				return
			}
		}
	}()

	<-ctx.Done()
	code, finished := s.docker.ExecExitCode(context.Background(), sess.ID)
	reason := "session ended"
	if finished {
		reason = "exit " + strconv.Itoa(code)
	}
	conn.Close(websocket.StatusNormalClosure, reason)
}
