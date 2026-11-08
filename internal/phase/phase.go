// Package phase runs a command as ordered Phases (see GLOSSARY.md). Each Phase judges
// its own completion from live state alone; nothing is stored between runs.
package phase

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// State is a Phase's state, judged from live state.
type State int

const (
	// Next: the Phase can run now.
	Next State = iota
	// Done: the Phase's outcome is already in place; it is skipped.
	Done
	// Blocked: a precondition is missing that the Phase can't fix itself.
	Blocked
	// Manual: a Manual checkpoint. Only the operator can complete it.
	Manual
)

func (s State) String() string {
	switch s {
	case Next:
		return "next"
	case Done:
		return "done"
	case Blocked:
		return "blocked"
	case Manual:
		return "manual"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// Status is a Phase's state and, unless it is Done or Next, why. For a Manual
// checkpoint, Reason is the instruction to the operator.
type Status struct {
	State  State
	Reason string
}

// Phase is one ordered step of a command.
type Phase interface {
	Name() string
	// Status judges the Phase from live state, without changing anything.
	Status(ctx context.Context) (Status, error)
	// Run brings the Phase to Done. It is only called when Status is Next.
	Run(ctx context.Context) error
}

// BlockedError stops a command at a Blocked Phase.
type BlockedError struct {
	Phase, Reason string
}

func (e *BlockedError) Error() string { return fmt.Sprintf("%s: blocked: %s", e.Phase, e.Reason) }

// CheckpointError stops a command at a Manual checkpoint, with its instruction.
type CheckpointError struct {
	Phase, Instruction string
}

func (e *CheckpointError) Error() string {
	return fmt.Sprintf("%s: manual checkpoint: %s", e.Phase, e.Instruction)
}

// UnknownPhaseError is a --from that names no Phase of the command.
type UnknownPhaseError struct {
	Name  string
	Known []string
}

func (e *UnknownPhaseError) Error() string {
	return fmt.Sprintf("no phase %q; the phases are %v", e.Name, e.Known)
}

// Run runs the phases in order, from the one named from (all when empty). Done phases
// are skipped; a Blocked phase or a Manual checkpoint stops the run with a
// *BlockedError or *CheckpointError. A phase that ran must be Done afterwards, or reach
// a Manual checkpoint. Progress goes to out.
func Run(ctx context.Context, phases []Phase, from string, out io.Writer) error {
	start, err := index(phases, from)
	if err != nil {
		return err
	}
	for _, p := range phases[start:] {
		st, err := p.Status(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name(), err)
		}
		if st.State == Next {
			_, _ = fmt.Fprintf(out, "%s: running\n", p.Name())
			if err := p.Run(ctx); err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
			if st, err = p.Status(ctx); err != nil {
				return fmt.Errorf("%s: %w", p.Name(), err)
			}
			if st.State == Next {
				return fmt.Errorf("%s: ran but is not done", p.Name())
			}
		}
		switch st.State {
		case Done:
			_, _ = fmt.Fprintf(out, "%s: done\n", p.Name())
		case Blocked:
			return &BlockedError{Phase: p.Name(), Reason: st.Reason}
		case Manual:
			return &CheckpointError{Phase: p.Name(), Instruction: st.Reason}
		}
	}
	return nil
}

// Line is one Phase in a status report.
type Line struct {
	Phase  string
	Status Status
}

// Report judges every phase without running any. A phase after one that isn't Done
// is judged on its own: its Status says what it would do if the earlier ones were.
func Report(ctx context.Context, phases []Phase) ([]Line, error) {
	lines := make([]Line, 0, len(phases))
	for _, p := range phases {
		st, err := p.Status(ctx)
		if err != nil {
			return lines, fmt.Errorf("%s: %w", p.Name(), err)
		}
		lines = append(lines, Line{Phase: p.Name(), Status: st})
	}
	return lines, nil
}

func index(phases []Phase, from string) (int, error) {
	if from == "" {
		return 0, nil
	}
	names := make([]string, len(phases))
	for i, p := range phases {
		if p.Name() == from {
			return i, nil
		}
		names[i] = p.Name()
	}
	return 0, &UnknownPhaseError{Name: from, Known: names}
}

// IsStop reports whether err is a stop the operator acts on (Blocked or a Manual
// checkpoint) rather than a failure.
func IsStop(err error) bool {
	var b *BlockedError
	var c *CheckpointError
	return errors.As(err, &b) || errors.As(err, &c)
}
