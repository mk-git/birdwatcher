//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestMain(m *testing.M) {
	// Disable the Ryuk reaper — it has issues with non-standard Docker socket
	// paths (e.g. Colima on macOS). Containers are cleaned up via t.Cleanup.
	os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	os.Exit(m.Run())
}

// initialBirdwatcherConf returns the placeholder match_route function written
// before the container starts. BIRD <= 2.13 (compatBird213) does not support the
// "-> bool" return-type syntax, so it must be omitted for those versions or BIRD
// fails to parse the config and never starts.
func initialBirdwatcherConf(compatBird213 bool) string {
	retType := " -> bool"
	if compatBird213 {
		retType = ""
	}
	return "# DO NOT EDIT MANUALLY\nfunction match_route()" + retType + "\n{\n\treturn false;\n}\n"
}

// testPrefixes are the two static routes defined in testdata/bird.conf
var testPrefixes = []string{"192.0.2.0/24", "198.51.100.0/24"}

// birdVersions is the matrix of BIRD versions the e2e flow runs against.
// compatBird213 must be true for versions before 2.14 (which introduced the
// "-> bool" function return type syntax).
var birdVersions = []struct {
	version       string
	compatBird213 bool
}{
	{"2.0.7", true},
	{"2.13", true},
	{"2.14", false},
	{"2.19.1", false},
}

// waitForRoutes polls birdc inside the container until all testPrefixes are
// either present (wantPresent=true) or all absent (wantPresent=false).
func waitForRoutes(t *testing.T, ctr testcontainers.Container, wantPresent bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := birdcShowRoute(t, ctr)
		if err == nil && routesPresent(output) == wantPresent {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	output, _ := birdcShowRoute(t, ctr)
	t.Fatalf("timeout waiting for routes present=%v; last birdc output:\n%s", wantPresent, output)
}

// birdcShowRoute runs 'birdc show route where match_route()' in the container
// and returns stdout as a string, or an error if the exec fails.
func birdcShowRoute(t *testing.T, ctr testcontainers.Container) (string, error) {
	t.Helper()
	ctx := context.Background()
	code, reader, err := ctr.Exec(ctx, []string{"birdc", "show route where match_route()"})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("birdc exited with code %d", code)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// routesPresent returns true if all testPrefixes appear in the birdc output.
func routesPresent(output string) bool {
	for _, p := range testPrefixes {
		if !strings.Contains(output, p) {
			return false
		}
	}
	return true
}

const birdwatcherConfTmpl = `configfile = "{{.TempDir}}/birdwatcher.conf"
reloadcommand = "/bin/sh {{.ReloadScript}}"
compatbird213 = {{.CompatBird213}}

[services]
  [services."test"]
  command = "test -f {{.TempDir}}/healthy"
  prefixes = ["192.0.2.0/24", "198.51.100.0/24"]
  interval = 1
  timeout = "2s"
`

func startBirdwatcher(t *testing.T, tempDir, containerID string, compatBird213 bool) {
	t.Helper()

	// Write a reload script that retries birdc configure until it succeeds to
	// allow virtiofs (Colima) to propagate the config-file rename from host to guest.
	reloadScript := filepath.Join(tempDir, "reload.sh")
	writeFile(t, reloadScript, "#!/bin/sh\ni=0\nwhile [ $i -lt 10 ]; do\n  docker exec "+containerID+" birdc configure && exit 0\n  i=$((i + 1))\n  sleep 0.5\ndone\nexit 1\n")

	// render birdwatcher config
	tmpl := template.Must(template.New("cfg").Parse(birdwatcherConfTmpl))
	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, struct {
		TempDir, ReloadScript string
		CompatBird213         bool
	}{tempDir, reloadScript, compatBird213}))

	cfgPath := filepath.Join(tempDir, "birdwatcher-e2e.conf")
	writeFile(t, cfgPath, buf.String())

	// repoRoot is the directory containing main.go, one level above e2e/
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")

	// Build birdwatcher into a temp binary so that killing the process also
	// stops all I/O (avoids "Test I/O incomplete" from go run's grandchild).
	binPath := filepath.Join(t.TempDir(), "birdwatcher")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = repoRoot
	require.NoError(t, build.Run(), "go build failed")

	cmd := exec.Command(binPath, "-config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func TestE2E(t *testing.T) {
	for _, bv := range birdVersions {
		t.Run(bv.version, func(t *testing.T) {
			t.Parallel()

			tempDir := dockerAccessibleTempDir(t)

			// The bind-mount replaces /etc/bird entirely, so both config files must
			// be written to tempDir before the container starts.
			birdConf, err := os.ReadFile(filepath.Join("testdata", "bird.conf"))
			require.NoError(t, err)
			writeFile(t, filepath.Join(tempDir, "bird.conf"), string(birdConf))
			writeFile(t, filepath.Join(tempDir, "birdwatcher.conf"), initialBirdwatcherConf(bv.compatBird213))

			ctr := startBIRD(t, tempDir, bv.version)
			t.Logf("container ID: %s", ctr.GetContainerID())

			// initial state: match_route returns false, no routes should appear
			waitForRoutes(t, ctr, false, 10*time.Second)
			t.Log("initial state confirmed: routes absent")

			startBirdwatcher(t, tempDir, ctr.GetContainerID(), bv.compatBird213)

			// Step 1: create healthy file — birdwatcher should add routes
			t.Log("creating healthy file")
			writeFile(t, filepath.Join(tempDir, "healthy"), "")
			waitForRoutes(t, ctr, true, 15*time.Second)
			t.Log("routes present: OK")

			// Step 2: remove healthy file — birdwatcher should withdraw routes
			t.Log("removing healthy file")
			require.NoError(t, os.Remove(filepath.Join(tempDir, "healthy")))
			waitForRoutes(t, ctr, false, 15*time.Second)
			t.Log("routes absent: OK")

			// Step 3: re-create healthy file — birdwatcher should re-add routes
			t.Log("re-creating healthy file")
			writeFile(t, filepath.Join(tempDir, "healthy"), "")
			waitForRoutes(t, ctr, true, 15*time.Second)
			t.Log("routes present again: OK")
		})
	}
}

// dockerAccessibleTempDir returns a temporary directory that is accessible inside
// the Docker VM. On macOS (e.g. with Colima), only the home directory is mounted
// by default, so we create a temp dir there instead of /var/folders (t.TempDir).
func dockerAccessibleTempDir(t *testing.T) string {
	t.Helper()
	base := ""
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		base = home
	}
	dir, err := os.MkdirTemp(base, "birdwatcher-e2e-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startBIRD(t *testing.T, tempDir, version string) testcontainers.Container {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    filepath.Join("testdata"),
			Dockerfile: "Dockerfile",
			BuildArgs: map[string]*string{
				"BIRD_VERSION": &version,
			},
		},
		Mounts: testcontainers.ContainerMounts{
			testcontainers.BindMount(tempDir, "/etc/bird"),
		},
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, ctr.Terminate(context.Background()))
	})

	return ctr
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}
