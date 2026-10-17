package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"homestack/internal/state"
)

// Preflight checks everything Homestack depends on. The first-run screen shows
// these before you create an account, and Settings > About shows them later.

type check struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	// Before setup this is public (there's no account to sign in with yet).
	// Afterwards it needs a session, since it reveals system details.
	var setupDone bool
	s.store.Read(func(st *state.State) { setupDone = st.Admin != nil })
	if setupDone {
		if _, ok := s.sessionFor(r); !ok {
			writeErr(w, http.StatusUnauthorized, "Sign in to continue.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	checks := s.runChecks(ctx)
	ok := true
	for _, c := range checks {
		if c.Status == "fail" {
			ok = false
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "checks": checks, "inContainer": inContainer()})
}

func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return os.Getenv("HOMESTACK_IN_CONTAINER") == "1"
}

var semverRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

func (s *Server) runChecks(ctx context.Context) []check {
	var out []check
	container := inContainer()

	// 1. Docker Engine reachable
	engine := check{ID: "engine", Label: "Docker Engine"}
	info, err := s.docker.Info(ctx)
	switch {
	case err != nil:
		engine.Status = "fail"
		engine.Detail = "Homestack can't talk to Docker (" + strings.TrimPrefix(err.Error(), "can't reach Docker: ") + ")."
		if container {
			engine.Fix = "Mount the Docker socket into the Homestack container:\n  volumes:\n    - /var/run/docker.sock:/var/run/docker.sock"
		} else {
			engine.Fix = "Install Docker (https://docs.docker.com/engine/install/) and make sure it's running:\n  sudo systemctl enable --now docker\nIf Docker is running, the user Homestack runs as needs access to the socket:\n  sudo usermod -aG docker $USER"
		}
	default:
		engine.Status = "ok"
		engine.Detail = fmt.Sprintf("Docker %s on %s (%s)", info.ServerVersion, info.OperatingSystem, info.Architecture)
		if m := semverRe.FindStringSubmatch(info.ServerVersion); m != nil {
			if major, _ := strconv.Atoi(m[1]); major < 23 {
				engine.Status = "warn"
				engine.Detail += ". This is quite old; Docker 24 or newer is recommended."
				engine.Fix = "Update Docker: https://docs.docker.com/engine/install/"
			}
		}
	}
	out = append(out, engine)

	// 2. Docker CLI
	cli := check{ID: "cli", Label: "Docker command-line tool"}
	cliPath, err := exec.LookPath("docker")
	if err != nil {
		cli.Status = "fail"
		cli.Detail = "The docker command isn't installed, and Homestack needs it to run Compose."
		if container {
			cli.Fix = "The official Homestack image includes it. If you built your own image, install docker-cli and docker-cli-compose."
		} else {
			cli.Fix = "Install Docker Engine, which includes the docker command:\n  https://docs.docker.com/engine/install/"
		}
	} else {
		cli.Status = "ok"
		cli.Detail = "Found at " + cliPath
	}
	out = append(out, cli)

	// 3. Docker Compose v2
	comp := check{ID: "compose", Label: "Docker Compose"}
	if cli.Status != "ok" {
		comp.Status = "fail"
		comp.Detail = "Needs the docker command first."
	} else {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		verOut, err := exec.CommandContext(cctx, "docker", "compose", "version", "--short").CombinedOutput()
		cancel()
		ver := strings.TrimSpace(string(verOut))
		if err != nil {
			comp.Status = "fail"
			comp.Detail = "The Compose plugin isn't installed (docker compose didn't run)."
			if _, lerr := exec.LookPath("docker-compose"); lerr == nil {
				comp.Detail += " Only the old docker-compose v1 was found, which isn't supported."
			}
			comp.Fix = "Install the Compose plugin:\n  Debian/Ubuntu: sudo apt install docker-compose-plugin\n  Fedora: sudo dnf install docker-compose-plugin\n  Other: https://docs.docker.com/compose/install/linux/"
		} else {
			comp.Status = "ok"
			comp.Detail = "Version " + ver
			if m := semverRe.FindStringSubmatch(ver); m != nil {
				major, _ := strconv.Atoi(m[1])
				minor, _ := strconv.Atoi(m[2])
				if major < 2 || (major == 2 && minor < 20) {
					comp.Status = "warn"
					comp.Detail += ". Version 2.20 or newer is recommended."
					comp.Fix = "Update the plugin: sudo apt install --only-upgrade docker-compose-plugin"
				}
			}
		}
	}
	out = append(out, comp)

	// 4. Stacks folder
	stacksDir := s.store.Settings().StacksDir
	sf := check{ID: "stacks", Label: "Stacks folder"}
	if err := os.MkdirAll(stacksDir, 0o755); err != nil {
		sf.Status = "fail"
		sf.Detail = fmt.Sprintf("Can't create %s: %v", stacksDir, err)
		sf.Fix = fmt.Sprintf("Create it and give Homestack access:\n  sudo mkdir -p %s\n  sudo chown $USER %s", stacksDir, stacksDir)
	} else if f, err := os.CreateTemp(stacksDir, ".homestack-write-test-"); err != nil {
		sf.Status = "fail"
		sf.Detail = fmt.Sprintf("%s exists but Homestack can't write to it.", stacksDir)
		sf.Fix = fmt.Sprintf("Give Homestack write access:\n  sudo chown -R $USER %s", stacksDir)
	} else {
		f.Close()
		os.Remove(f.Name())
		_, dirs := s.stackDirs()
		sf.Status = "ok"
		sf.Detail = fmt.Sprintf("%s (%d existing stacks found)", stacksDir, len(dirs))
	}
	out = append(out, sf)

	// 5. Container mounts: the stacks folder must be at the same path inside
	// and outside, because Compose passes paths like ./data to the Docker
	// daemon, which resolves them on the host.
	if container && engine.Status != "fail" {
		out = append(out, s.checkMounts(ctx, stacksDir))
	}

	// 6. Virtual machines (optional)
	vmc := check{ID: "vms", Label: "Virtual machines (optional)", Status: "ok"}
	vctx, vcancel := context.WithTimeout(ctx, 8*time.Second)
	if ver, err := s.vms.Status(vctx); err == nil {
		vmc.Detail = "Managing VMs through " + ver
	} else {
		vmc.Detail = "Not set up, so the VMs page is hidden. To manage KVM virtual machines, install libvirt on the host"
		if container {
			vmc.Detail += " and mount /var/run/libvirt into this container"
		}
		vmc.Detail += "."
	}
	vcancel()
	out = append(out, vmc)

	// 7. Data folder persistence
	data := check{ID: "data", Label: "Settings storage", Status: "ok", Detail: s.store.Path()}
	if container && !isMountPoint(filepath.Dir(s.store.Path())) {
		data.Status = "warn"
		data.Detail = s.store.Path() + " isn't on a mounted volume, so your account and settings will be lost if the container is recreated."
		data.Fix = "Add a volume for the data folder:\n  volumes:\n    - ./data:/data"
	}
	out = append(out, data)
	return out
}

// checkMounts inspects Homestack's own container.
func (s *Server) checkMounts(ctx context.Context, stacksDir string) check {
	c := check{ID: "mounts", Label: "Container mounts"}
	raw, err := s.docker.ContainerInspect(ctx, selfContainerID())
	if err != nil {
		c.Status = "warn"
		c.Detail = "Couldn't inspect Homestack's own container to check its mounts."
		return c
	}
	var v struct {
		Mounts []struct {
			Source      string `json:"Source"`
			Destination string `json:"Destination"`
		} `json:"Mounts"`
		HostConfig struct {
			NetworkMode string `json:"NetworkMode"`
		} `json:"HostConfig"`
	}
	_ = json.Unmarshal(raw, &v)
	var match *struct{ src, dst string }
	for _, m := range v.Mounts {
		if filepath.Clean(m.Destination) == filepath.Clean(stacksDir) {
			match = &struct{ src, dst string }{m.Source, m.Destination}
		}
	}
	fix := fmt.Sprintf("Mount the stacks folder at the same path on both sides:\n  volumes:\n    - %s:%s", stacksDir, stacksDir)
	switch {
	case match == nil:
		c.Status = "fail"
		c.Detail = fmt.Sprintf("%s isn't mounted from the host, so stacks would be lost and relative paths like ./data wouldn't work.", stacksDir)
		c.Fix = fix
	case filepath.Clean(match.src) != filepath.Clean(match.dst):
		c.Status = "warn"
		c.Detail = fmt.Sprintf("The stacks folder is mounted from %s on the host but appears as %s inside. Relative paths like ./config in compose files will point to the wrong place.", match.src, match.dst)
		c.Fix = fix
	default:
		c.Status = "ok"
		c.Detail = fmt.Sprintf("%s is mounted at the same path inside and out", stacksDir)
	}
	if c.Status == "ok" && v.HostConfig.NetworkMode != "host" {
		c.Status = "warn"
		c.Detail += ". The container isn't using host networking, so the \"Listen on\" setting will only see the container's own interfaces."
		c.Fix = "For full control over which interfaces Homestack listens on, add:\n  network_mode: host"
	}
	return c
}

var containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// selfContainerID finds our own container ID. The hostname is usually the
// short ID, but not with network_mode: host, so check the mounts Docker
// creates for /etc/hostname first.
func selfContainerID() string {
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := containerIDRe.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	host, _ := os.Hostname()
	return host
}

// isMountPoint reports whether dir is a separate mount (i.e. a volume).
func isMountPoint(dir string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return true // can't tell; don't warn
	}
	dir = filepath.Clean(dir)
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 4 && filepath.Clean(f[4]) == dir {
			return true
		}
	}
	return false
}
