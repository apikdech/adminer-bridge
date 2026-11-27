package secrets_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/apikdech/adminer-bridge/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSecrets writes content to a temp file and sets its mode exactly.
func writeSecrets(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.env")
	require.NoError(t, os.WriteFile(p, []byte(content), mode))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

func TestLoadParsesCommonForms(t *testing.T) {
	p := writeSecrets(t, 0o600, "# comment\n\nA=plain\nexport B=exported\nC=\"double quoted\"\nD='single quoted'\nE=has=equals\n")
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		t.Setenv(k, "")
	}
	require.NoError(t, secrets.Load(p))
	assert.Equal(t, "plain", os.Getenv("A"))
	assert.Equal(t, "exported", os.Getenv("B"))
	assert.Equal(t, "double quoted", os.Getenv("C"))
	assert.Equal(t, "single quoted", os.Getenv("D"))
	assert.Equal(t, "has=equals", os.Getenv("E"))
}

func TestLoadShellValueWins(t *testing.T) {
	p := writeSecrets(t, 0o600, "A=file\n")
	t.Setenv("A", "shell")
	require.NoError(t, secrets.Load(p))
	assert.Equal(t, "shell", os.Getenv("A"))
}

func TestLoadRefusesReadableByOthers(t *testing.T) {
	p := writeSecrets(t, 0o644, "A=x\n")
	err := secrets.Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), p)
	assert.Contains(t, err.Error(), "chmod 600")
}

func TestLoadRejectsBadLineWithoutEchoingIt(t *testing.T) {
	p := writeSecrets(t, 0o600, "A=ok\nnot a pair supersecret\n")
	err := secrets.Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2")
	assert.NotContains(t, err.Error(), "supersecret")
}

func TestLoadMissingFile(t *testing.T) {
	err := secrets.Load(filepath.Join(t.TempDir(), "none.env"))
	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}
