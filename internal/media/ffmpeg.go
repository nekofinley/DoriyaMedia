package media

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// FFMpeg wraps the ffmpeg binary.
type FFMpeg struct {
	Path string
	// egress is the process-wide set of administrator-trusted LAN origins.
	// Every remote input still gets its own ephemeral controlled proxy.
	egress *egressTrust
}

func (f *FFMpeg) RunStream(ctx context.Context, args []string, out io.Writer) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args, controller, err := secureFFmpegArgs(
		args, egressTrustFromContext(cctx, f.egress))
	if err != nil {
		return err
	}
	if controller != nil {
		defer controller.Close()
	}
	cmd := exec.CommandContext(cctx, f.Path, args...)
	cmd.Env = secureMediaEnv(controller.DefaultProxyURL())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	doneCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(out, stdout)
		doneCh <- err
	}()
	var errLines []string
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	go func() {
		for sc.Scan() {
			if len(errLines) < 8 {
				errLines = append(errLines, sc.Text())
			}
		}
		doneCh <- nil
	}()
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.Join(errLines, " | "))
	}
	return nil
}

// HeaderArgs renders HTTP headers for ffmpeg's -headers option.
func HeaderArgs(args []string, headers map[string]string) []string {
	if len(headers) == 0 {
		return args
	}
	var b strings.Builder
	for k, v := range headers {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}
	args = append(args, "-headers", b.String())
	return args
}

// ProxyArgs tells ffmpeg to reach the input through a proxy. The value is an
// http:// URL — the only form ffmpeg's own http protocol honours, which is why a
// SOCKS5 or Shadowsocks source is handed a loopback bridge instead (see
// netproxy.Bridge). It is an input option, so it goes before -i.
func ProxyArgs(args []string, proxy string) []string {
	if strings.TrimSpace(proxy) == "" {
		return args
	}
	return append(args, "-http_proxy", proxy)
}
