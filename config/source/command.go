package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nrynss/keel/config"
)

// waitDelay bounds how long Wait blocks on an inherited pipe after the
// context kills a child.
const waitDelay = 5 * time.Second

// maxStderr is how many bytes of standard error a failed command keeps.
// A command that dumps without bound cannot grow the error without limit.
const maxStderr = 512

// command resolves a secret by running the reference's program and
// reading standard output. It never passes a value as an argument.
type command struct{}

func (command) Resolve(ctx context.Context, ref config.Ref) (string, error) {
	ctx, err := startCtx(ctx, "command", ref)
	if err != nil {
		return "", err
	}
	if ref.Command() == "" {
		return "", annotate(ErrMissingLocator, "command", ref)
	}
	cmd := exec.CommandContext(ctx, ref.Command(), ref.Args()...)
	cmd.WaitDelay = waitDelay
	var stdout bytes.Buffer
	var stderr capBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", annotate(ctx.Err(), "command", ref)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", annotate(ErrNotFound, "command", ref)
		}
		msg := strings.TrimSpace(stderr.bytes())
		if stderr.truncated() {
			msg = msg + " (truncated)"
		}
		if msg != "" {
			return "", annotate(fmt.Errorf("%w: %s", ErrCommand, msg), "command", ref)
		}
		return "", annotate(ErrCommand, "command", ref)
	}
	return trimOneNewline(stdout.String()), nil
}

// capBuffer keeps the first maxStderr bytes written to it and reports
// whether it dropped the rest. Write always accepts the full input so a
// child that writes without bound does not block on a closed pipe.
type capBuffer struct {
	buf []byte
	hit bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= maxStderr {
		b.hit = true
		return len(p), nil
	}
	need := maxStderr - len(b.buf)
	if len(p) > need {
		b.buf = append(b.buf, p[:need]...)
		b.hit = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *capBuffer) bytes() string {
	return string(b.buf)
}

func (b *capBuffer) truncated() bool {
	return b.hit
}
