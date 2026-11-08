package phase

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

// fake is a Phase whose state the test sets; Run moves it to after.
type fake struct {
	name   string
	state  Status
	after  Status
	runErr error
	ran    *[]string
}

func (f *fake) Name() string                           { return f.name }
func (f *fake) Status(context.Context) (Status, error) { return f.state, nil }
func (f *fake) Run(context.Context) error {
	*f.ran = append(*f.ran, f.name)
	if f.runErr != nil {
		return f.runErr
	}
	f.state = f.after
	return nil
}

func phases(ran *[]string, fs ...*fake) []Phase {
	out := make([]Phase, len(fs))
	for i, f := range fs {
		f.ran = ran
		out[i] = f
	}
	return out
}

var done = Status{State: Done}

func TestRunOrderAndSkip(t *testing.T) {
	var ran []string
	ps := phases(&ran,
		&fake{name: "a", state: done},
		&fake{name: "b", state: Status{State: Next}, after: done},
		&fake{name: "c", state: Status{State: Next}, after: done},
	)
	if err := Run(t.Context(), ps, "", io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"b", "c"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	// A second run changes nothing.
	ran = nil
	if err := Run(t.Context(), ps, "", io.Discard); err != nil || len(ran) != 0 {
		t.Errorf("second run: ran %v, err %v", ran, err)
	}
}

func TestRunStopsAtBlocked(t *testing.T) {
	var ran []string
	ps := phases(&ran,
		&fake{name: "a", state: Status{State: Blocked, Reason: "no Docker"}},
		&fake{name: "b", state: Status{State: Next}, after: done},
	)
	err := Run(t.Context(), ps, "", io.Discard)
	var b *BlockedError
	if !errors.As(err, &b) || b.Phase != "a" || b.Reason != "no Docker" || !IsStop(err) {
		t.Fatalf("Run: %v, want a *BlockedError for a", err)
	}
	if len(ran) != 0 {
		t.Errorf("ran %v after a blocked phase", ran)
	}
}

func TestRunStopsAtManualCheckpoint(t *testing.T) {
	var ran []string
	ps := phases(&ran,
		&fake{name: "a", state: Status{State: Next}, after: Status{State: Manual, Reason: "add the deploy key"}},
		&fake{name: "b", state: Status{State: Next}, after: done},
	)
	err := Run(t.Context(), ps, "", io.Discard)
	var c *CheckpointError
	if !errors.As(err, &c) || c.Phase != "a" || c.Instruction != "add the deploy key" || !IsStop(err) {
		t.Fatalf("Run: %v, want a *CheckpointError for a", err)
	}
	if want := []string{"a"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
}

func TestRunFrom(t *testing.T) {
	var ran []string
	ps := phases(&ran,
		&fake{name: "a", state: Status{State: Blocked, Reason: "skipped by --from"}},
		&fake{name: "b", state: Status{State: Next}, after: done},
	)
	if err := Run(t.Context(), ps, "b", io.Discard); err != nil {
		t.Fatalf("Run --from b: %v", err)
	}
	if want := []string{"b"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	var u *UnknownPhaseError
	if err := Run(t.Context(), ps, "x", io.Discard); !errors.As(err, &u) || IsStop(err) {
		t.Errorf("Run --from x: %v, want an *UnknownPhaseError", err)
	}
}

func TestRunFailures(t *testing.T) {
	var ran []string
	boom := errors.New("boom")
	if err := Run(t.Context(), phases(&ran, &fake{name: "a", state: Status{State: Next}, runErr: boom}), "", io.Discard); !errors.Is(err, boom) || IsStop(err) {
		t.Errorf("Run with a failing phase: %v", err)
	}
	if err := Run(t.Context(), phases(&ran, &fake{name: "a", state: Status{State: Next}, after: Status{State: Next}}), "", io.Discard); err == nil {
		t.Error("a phase still Next after Run must fail the run")
	}
}

func TestReport(t *testing.T) {
	var ran []string
	ps := phases(&ran,
		&fake{name: "a", state: done},
		&fake{name: "b", state: Status{State: Next}},
		&fake{name: "c", state: Status{State: Blocked, Reason: "needs b"}},
	)
	lines, err := Report(t.Context(), ps)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(lines))
	for i, l := range lines {
		got[i] = l.Phase + "=" + l.Status.State.String()
	}
	if want := []string{"a=done", "b=next", "c=blocked"}; !slices.Equal(got, want) {
		t.Errorf("Report: %v, want %v", got, want)
	}
	if len(ran) != 0 {
		t.Errorf("Report ran %v", ran)
	}
}
