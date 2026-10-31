package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.HasPrefix(out.String(), "go-template dev\n") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}
