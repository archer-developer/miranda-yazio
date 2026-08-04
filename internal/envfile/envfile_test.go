package envfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_MissingFileIsNotError(t *testing.T) {
	err := Load(filepath.Join(t.TempDir(), "does-not-exist.env"))
	assert.NoError(t, err)
}

func TestLoad_SetsUnsetVariable(t *testing.T) {
	path := writeEnvFile(t, "FOO=bar\n")
	unsetEnv(t, "FOO")

	require.NoError(t, Load(path))
	assert.Equal(t, "bar", os.Getenv("FOO"))
}

func TestLoad_DoesNotOverrideExistingEnv(t *testing.T) {
	path := writeEnvFile(t, "FOO=from-file\n")
	t.Setenv("FOO", "from-environment")

	require.NoError(t, Load(path))
	assert.Equal(t, "from-environment", os.Getenv("FOO"))
}

func TestLoad_SkipsCommentsAndBlankLines(t *testing.T) {
	path := writeEnvFile(t, "# a comment\n\nFOO=bar\n")
	unsetEnv(t, "FOO")

	require.NoError(t, Load(path))
	assert.Equal(t, "bar", os.Getenv("FOO"))
}

func TestLoad_StripsExportPrefixAndQuotes(t *testing.T) {
	path := writeEnvFile(t, "export FOO=\"quoted value\"\n")
	unsetEnv(t, "FOO")

	require.NoError(t, Load(path))
	assert.Equal(t, "quoted value", os.Getenv("FOO"))
}

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// unsetEnv unsets key for the duration of the test and restores its previous
// value (or absence) afterward, since t.Setenv cannot express "unset".
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, existed := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
