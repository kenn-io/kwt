package shell

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteWrapper_Bash(t *testing.T) {
	var buf bytes.Buffer
	err := WriteWrapper(&buf, "bash", TemplateData{CommandName: "kwt"})
	if err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "kwt()") {
		t.Error("bash wrapper should contain kwt() function")
	}
	if !strings.Contains(output, "__KWT_CD_SHIM=1") {
		t.Error("bash wrapper should contain __KWT_CD_SHIM=1")
	}
	if !strings.Contains(output, "builtin cd") {
		t.Error("bash wrapper should contain builtin cd")
	}
}

func TestWriteWrapper_Zsh(t *testing.T) {
	var buf bytes.Buffer
	err := WriteWrapper(&buf, "zsh", TemplateData{CommandName: "kwt"})
	if err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "kwt()") {
		t.Error("zsh wrapper should contain kwt() function")
	}
	if !strings.Contains(output, "cd)") {
		t.Error("zsh wrapper should dispatch cd")
	}
	if !strings.Contains(output, "__KWT_CD_SHIM=1") {
		t.Error("zsh wrapper should contain __KWT_CD_SHIM=1")
	}
}

func TestWriteWrapper_Fish(t *testing.T) {
	var buf bytes.Buffer
	err := WriteWrapper(&buf, "fish", TemplateData{CommandName: "kwt"})
	if err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "function kwt") {
		t.Error("fish wrapper should contain function kwt")
	}
	if !strings.Contains(output, "set -lx __KWT_CD_SHIM 1") {
		t.Error("fish wrapper should contain set -lx __KWT_CD_SHIM 1")
	}
}

func TestWriteWrapper_InvalidShell(t *testing.T) {
	var buf bytes.Buffer
	err := WriteWrapper(&buf, "powershell", TemplateData{CommandName: "kwt"})
	if err == nil {
		t.Error("WriteWrapper() should return error for unsupported shell")
	}
	if !strings.Contains(err.Error(), "unsupported shell") {
		t.Errorf("error should mention unsupported shell, got: %v", err)
	}
}

func TestWriteWrapper_CustomCommandName(t *testing.T) {
	var buf bytes.Buffer
	err := WriteWrapper(&buf, "bash", TemplateData{CommandName: "mykwt"})
	if err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "mykwt()") {
		t.Error("wrapper should use custom command name")
	}
}

func TestBashSyntaxValid(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	var buf bytes.Buffer
	if err := WriteWrapper(&buf, "bash", TemplateData{CommandName: "kwt"}); err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-n", "-c", buf.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("bash syntax check failed: %v\n%s", err, output)
	}
}

func TestZshSyntaxValid(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}
	var buf bytes.Buffer
	if err := WriteWrapper(&buf, "zsh", TemplateData{CommandName: "kwt"}); err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "zsh", "-n", "-c", buf.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("zsh syntax check failed: %v\n%s", err, output)
	}
}

func TestFishSyntaxValid(t *testing.T) {
	if _, err := exec.LookPath("fish"); err != nil {
		t.Skip("fish not available")
	}
	var buf bytes.Buffer
	if err := WriteWrapper(&buf, "fish", TemplateData{CommandName: "kwt"}); err != nil {
		t.Fatalf("WriteWrapper() error = %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "fish", "--no-execute", "-c", buf.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("fish syntax check failed: %v\n%s", err, output)
	}
}

func TestShimWrapsCdOnly(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		var buf bytes.Buffer
		require.NoError(t, WriteWrapper(&buf, sh, TemplateData{CommandName: "kwt"}))
		out := buf.String()
		assert.Contains(t, out, "cd)", sh)
		assert.NotContains(t, out, "cd|add)", sh)
	}

	var fishBuf bytes.Buffer
	require.NoError(t, WriteWrapper(&fishBuf, "fish", TemplateData{CommandName: "kwt"}))
	assert.NotContains(t, fishBuf.String(), "case cd add")
}
