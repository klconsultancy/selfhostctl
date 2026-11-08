package envsubst

import (
	"slices"
	"strings"
	"testing"
)

func lookup(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestRefs(t *testing.T) {
	got := Refs("a: ${B}\nb: ${A} ${B} $C ${1X} ${_OK}")
	if want := []string{"A", "B", "_OK"}; !slices.Equal(got, want) {
		t.Errorf("Refs: %v, want %v", got, want)
	}
}

func TestExpand(t *testing.T) {
	out, values, err := Expand("token: ${TOKEN}\nkeep: $TOKEN ${", lookup(map[string]string{"TOKEN": "s3cret"}))
	if err != nil || out != "token: s3cret\nkeep: $TOKEN ${" || !slices.Equal(values, []string{"s3cret"}) {
		t.Errorf("Expand: %q %v %v", out, values, err)
	}
	_, _, err = Expand("${B} ${A} ${EMPTY}", lookup(map[string]string{"EMPTY": ""}))
	if err == nil || !strings.Contains(err.Error(), "A, B, EMPTY") {
		t.Errorf("Expand with missing variables: %v, want all three named", err)
	}
}

func TestRedact(t *testing.T) {
	got := Redact(`Secret "x" is invalid: data.token: "s3cret-long" and "s3cret"`, []string{"s3cret", "s3cret-long", "ab"})
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "<redacted>") {
		t.Errorf("Redact: %q", got)
	}
}
