package browser

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Docker launches browsers with the docker CLI, for local development. Each
// container publishes its VNC port on a random loopback port.
type Docker struct {
	// Image is the browser image, for example a local build of browser/.
	Image string
	// Binary is the CLI to run. Default "docker"; "podman" works too.
	Binary string
	// ExtraArgs are added to `docker run`, for example a seccomp profile
	// or --network.
	ExtraArgs []string
	// Env adds container environment variables, for example
	// BROWSER_NO_SANDBOX=1 where the runtime blocks Chromium's sandbox.
	Env map[string]string
}

// Launch implements Launcher.
func (d *Docker) Launch(ctx context.Context, s Spec) (Instance, error) {
	if _, err := ValidateURL(s.URL); err != nil {
		return nil, err
	}
	s = s.withDefaults()
	args := []string{"run", "-d", "--rm",
		"-p", fmt.Sprintf("127.0.0.1::%d", VNCPort),
		"--shm-size", "256m",
		"--label", "bmc-console.browser=1",
		"--label", "bmc-console.session=" + s.Session,
		"--stop-timeout", "2",
	}
	env := maps.Clone(d.Env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, s.env())
	for _, k := range slices.Sorted(maps.Keys(env)) {
		// Passing only the name makes docker read the value from the
		// environment of the CLI process, so it is not on its command line.
		args = append(args, "-e", k)
	}
	args = append(args, d.ExtraArgs...)
	args = append(args, d.Image)

	cmd := exec.CommandContext(ctx, d.binary(), args...)
	cmd.Env = append(cmd.Environ(), envList(env)...)
	out, err := run(cmd)
	if err != nil {
		return nil, fmt.Errorf("browser: docker run: %w", err)
	}
	id := strings.TrimSpace(out)
	inst := &dockerInstance{d: d, id: id}
	// Remove the container after MaxLifetime even if the session hangs.
	inst.timer = time.AfterFunc(s.MaxLifetime, func() { inst.Close() })

	port, err := run(exec.CommandContext(ctx, d.binary(), "port", id, fmt.Sprintf("%d/tcp", VNCPort)))
	if err != nil {
		inst.Close()
		return nil, fmt.Errorf("browser: docker port: %w", err)
	}
	// "127.0.0.1:49153", possibly followed by an IPv6 line.
	line, _, _ := strings.Cut(strings.TrimSpace(port), "\n")
	host, p, err := net.SplitHostPort(strings.TrimSpace(line))
	if err != nil {
		inst.Close()
		return nil, fmt.Errorf("browser: docker port %q: %w", line, err)
	}
	inst.addr = net.JoinHostPort(host, p)
	return inst, nil
}

func (d *Docker) binary() string {
	if d.Binary == "" {
		return "docker"
	}
	return d.Binary
}

type dockerInstance struct {
	d     *Docker
	id    string
	addr  string
	timer *time.Timer
	once  sync.Once
	err   error
}

func (i *dockerInstance) Addr() string { return i.addr }

func (i *dockerInstance) Close() error {
	i.once.Do(func() {
		i.timer.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, i.err = run(exec.CommandContext(ctx, i.d.binary(), "rm", "-f", i.id))
	})
	return i.err
}

func run(cmd *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
