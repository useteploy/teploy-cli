package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/useteploy/teploy/internal/ssh"
	"time"
)

// PublicationError means an error was reported after authority may have changed.
// Destructive compensation is unsafe when Committed or Unknown is true.
type PublicationError struct {
	Committed, Unknown bool
	Err                error
}

func (e *PublicationError) Error() string {
	return fmt.Sprintf("state publication needs reconciliation (committed=%t unknown=%t): %v", e.Committed, e.Unknown, e.Err)
}
func (e *PublicationError) Unwrap() error { return e.Err }
func PreservePublishedState(err error) bool {
	var e *PublicationError
	return errors.As(err, &e) && (e.Committed || e.Unknown)
}

func resolvePublication(exec ssh.Executor, app string, data []byte, generation uint64, lk *Lock, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actual, present, err := ReadRemoteFile(ctx, exec, fmt.Sprintf("%s/%s/state.json", deploymentsDir, app))
	if err != nil {
		return &PublicationError{Unknown: true, Err: errors.Join(cause, err)}
	}
	if !present {
		return cause
	}
	if !bytes.Equal(bytes.TrimSpace(actual), bytes.TrimSpace(data)) {
		// A different, parseable predecessor proves this write did not publish.
		prior, readErr := Read(ctx, exec, app)
		if readErr == nil && prior != nil && prior.Generation < generation {
			return cause
		}
		return &PublicationError{Unknown: true, Err: cause}
	}
	if lk != nil {
		// Repair only while our lease still protects this exact authority.
		if err := lk.Check(ctx, exec); err != nil {
			return &PublicationError{Committed: true, Err: errors.Join(cause, err)}
		}
	}
	if err := writeGenerationSidecarFenced(ctx, exec, app, generation, lk); err != nil {
		return &PublicationError{Committed: true, Err: errors.Join(cause, err)}
	}
	return nil
}

// GenerationExpectation binds application of a reviewed plan to its baseline.
type GenerationExpectation struct {
	Exists     bool
	Generation uint64
}

func (e *GenerationExpectation) Check(app string, current *AppState) error {
	if e == nil {
		return nil
	}
	if e.Exists != (current != nil) || (current != nil && current.Generation != e.Generation) {
		return fmt.Errorf("plan baseline changed for %s; review a fresh plan", app)
	}
	return nil
}
