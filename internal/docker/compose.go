package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ComposeFileNames are checked in this order, matching `docker compose`.
var ComposeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// FindComposeFile returns the compose file in dir, or "" if there isn't one.
func FindComposeFile(dir string) string {
	for _, n := range ComposeFileNames {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
			return n
		}
	}
	return ""
}

// Compose runs `docker compose <args>` in dir and streams every output line
// to out. It returns an error if the command fails.
func Compose(ctx context.Context, dir string, args []string, out func(string)) error {
	full := append([]string{"compose", "--ansi", "never", "--progress", "plain"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COMPOSE_STATUS_STDOUT=1")
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("can't run docker compose: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			out(strings.TrimRight(sc.Text(), "\r"))
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("docker compose %s failed (%v)", args[0], err)
	}
	return nil
}

// ValidateCompose checks an unsaved compose file (and .env) without touching
// the real files. It returns compose's own error message if it's invalid.
func ValidateCompose(ctx context.Context, dir, composeYAML, envText string) error {
	tmp, err := os.MkdirTemp("", "homestack-validate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = tmp // a new stack whose folder doesn't exist yet
	}
	cf := filepath.Join(tmp, "compose.yaml")
	if err := os.WriteFile(cf, []byte(composeYAML), 0o600); err != nil {
		return err
	}
	ef := filepath.Join(tmp, ".env")
	if err := os.WriteFile(ef, []byte(envText), 0o600); err != nil {
		return err
	}
	var lines []string
	err = Compose(ctx, dir, []string{"-f", cf, "--project-directory", dir, "--env-file", ef, "config", "--quiet"}, func(l string) {
		lines = append(lines, l)
	})
	if err != nil {
		msg := strings.TrimSpace(strings.Join(lines, "\n"))
		msg = strings.ReplaceAll(msg, cf, "compose.yaml")
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
