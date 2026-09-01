// Package daemon implements the three-stage idle state machine.
//
// Stages, all timed from when the session goes idle. Any user activity at any
// stage tears everything down and re-arms from zero. See docs/spec.md sections 3 and 6.3.
//
//	saver   SAVER_DELAY                             launch a random module
//	lock    SAVER_DELAY + LOCK_AFTER                stop the module, lock the session
//	blank   SAVER_DELAY + LOCK_AFTER + BLANK_AFTER  power the display off
package daemon

import (
	"context"
	"errors"

	"github.com/c-premus/retrosaver/internal/config"
)

// ErrNotImplemented is returned by every stub in this package.
var ErrNotImplemented = errors.New("daemon: not implemented")

// Daemon owns the idle watches and the currently running saver.
type Daemon struct {
	cfg config.Config
}

// New returns a Daemon configured from cfg.
func New(cfg config.Config) *Daemon { return &Daemon{cfg: cfg} }

// Run arms the watches and blocks until ctx is cancelled.
//
// On shutdown it must tear down the saver and restore idle-delay, including on
// an unclean exit, which is why the systemd unit also carries ExecStopPost.
func (d *Daemon) Run(ctx context.Context) error { return ErrNotImplemented }
