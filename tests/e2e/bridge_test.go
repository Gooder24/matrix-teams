// Package e2e runs the bridge as its own process between a throwaway Tuwunel homeserver
// and the fake Microsoft in internal/fakegraph. Nothing reaches Microsoft or another server.
//
//	TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Gooder24/matrix-teams/internal/fakegraph"
)

const server = "test"

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func secret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// until waits for something the bridge does on its own (a poll, a sync) and reports whether it happened.
func until(d time.Duration, check func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if check() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return check()
}

func ghost(oid string) string { return "@teams_" + oid + ":" + server }

type env struct {
	t        *testing.T
	fake     *fakegraph.Server
	c        *mx
	prov     string
	provAuth string
	pid      int
}

func (e *env) provision(method, path string, body any, out any) error {
	var reader *bytes.Reader
	data, _ := json.Marshal(body)
	reader = bytes.NewReader(data)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, _ := http.NewRequest(method, e.prov+path+sep+"user_id="+url.QueryEscape(e.c.userID), reader)
	req.Header.Set("Authorization", "Bearer "+e.provAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (e *env) writes(method, pathPart string) []fakegraph.Entry {
	var out []fakegraph.Entry
	for _, entry := range e.fake.Log() {
		if entry.Method == method && strings.Contains(entry.Path, pathPart) {
			out = append(out, entry)
		}
	}
	return out
}

func setup(t *testing.T, poll string) *env {
	tuwunel := os.Getenv("TUWUNEL")
	if tuwunel == "" {
		t.Skip("TUWUNEL is not set")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "matrix-teams")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/matrix-teams")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}
	fake := fakegraph.New()
	t.Cleanup(fake.Close)
	// Tokens live 30 s, so the bridge refreshes before every call and the rotation of refresh tokens is exercised.
	fake.ExpiresIn = 30

	hsPort, brPort := freePort(t), freePort(t)
	asToken, hsToken, regToken, provSecret := secret(), secret(), secret(), secret()
	apps := filepath.Join(dir, "appservices")
	_ = os.Mkdir(apps, 0o700)
	registration := fmt.Sprintf(`id: teams
url: http://127.0.0.1:%d
as_token: %s
hs_token: %s
sender_localpart: teamsbot
rate_limited: false
receive_ephemeral: true
de.sorunome.msc2409.push_ephemeral: true
namespaces:
  users:
  - regex: '^@teamsbot:%s$'
    exclusive: true
  - regex: '^@teams_.*:%s$'
    exclusive: true
`, brPort, asToken, hsToken, server, server)
	must(t, os.WriteFile(filepath.Join(apps, "teams.yaml"), []byte(registration), 0o600))
	hsConf := fmt.Sprintf(`[global]
server_name = "%s"
database_path = "%s"
address = ["127.0.0.1"]
port = %d
allow_registration = true
registration_token = "%s"
allow_federation = false
new_user_displayname_suffix = ""
log = "warn"
log_colors = false
appservice_dir = "%s"
`, server, filepath.Join(dir, "hsdb"), hsPort, regToken, apps)
	must(t, os.WriteFile(filepath.Join(dir, "tuwunel.toml"), []byte(hsConf), 0o600))
	hsLog, _ := os.Create(filepath.Join(dir, "tuwunel.log"))
	hs := exec.Command(tuwunel, "-c", filepath.Join(dir, "tuwunel.toml"))
	hs.Stdout, hs.Stderr = hsLog, hsLog
	must(t, hs.Start())
	t.Cleanup(func() { _ = hs.Process.Kill(); _ = hs.Wait() })
	hsURL := fmt.Sprintf("http://127.0.0.1:%d", hsPort)
	if !until(30*time.Second, func() bool {
		resp, err := http.Get(hsURL + "/_matrix/client/versions")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	}) {
		t.Fatal("Tuwunel did not start")
	}

	c := &mx{t: t, base: hsURL, events: map[string][]map[string]any{}, stop: make(chan struct{})}
	c.register("alex", regToken)
	go c.syncLoop()
	t.Cleanup(func() { close(c.stop) })

	config := fmt.Sprintf(`network:
    graph_url: %s
    login_url: %s
    client_id: test-client
    tenant: organizations
    poll_interval: %s
    initial_messages: 20
    initial_days: 14
    max_file_size: 26214400
bridge:
    personal_filtering_spaces: false
    permissions:
        "%s": admin
database:
    type: sqlite3-fk-wal
    uri: file:%s?_txlock=immediate
homeserver:
    address: %s
    domain: %s
appservice:
    address: http://127.0.0.1:%d
    hostname: 127.0.0.1
    port: %d
    id: teams
    bot:
        username: teamsbot
    as_token: %s
    hs_token: %s
    username_template: teams_{{.}}
provisioning:
    shared_secret: %s
logging:
    min_level: debug
    writers:
    - type: stdout
      format: pretty
`, fake.G(), fake.URL, poll, c.userID, filepath.Join(dir, "bridge.db"), hsURL, server, brPort, brPort, asToken, hsToken, provSecret)
	must(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600))
	brLogPath := filepath.Join(dir, "bridge.log")
	brLog, _ := os.Create(brLogPath)
	br := exec.Command(bin, "-c", filepath.Join(dir, "config.yaml"), "-n")
	br.Stdout, br.Stderr = brLog, brLog
	must(t, br.Start())
	t.Cleanup(func() {
		_ = br.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = br.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = br.Process.Kill()
		}
		data, _ := os.ReadFile(brLogPath)
		if keep := os.Getenv("E2E_LOG"); keep != "" {
			_ = os.WriteFile(keep, data, 0o600)
		}
		if t.Failed() {
			lines := strings.Split(string(data), "\n")
			t.Logf("bridge log (last 60 lines):\n%s", strings.Join(lines[max(0, len(lines)-60):], "\n"))
		}
	})
	e := &env{t: t, fake: fake, c: c, pid: br.Process.Pid, prov: fmt.Sprintf("http://127.0.0.1:%d/_matrix/provision", brPort), provAuth: provSecret}
	if !until(30*time.Second, func() bool { return e.provision("GET", "/v3/whoami", nil, nil) == nil }) {
		t.Fatal("bridge did not start")
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type loginStep struct {
	LoginID      string `json:"login_id"`
	Type         string `json:"type"`
	StepID       string `json:"step_id"`
	Instructions string `json:"instructions"`
}

var reState = regexp.MustCompile(`[?&]state=([^&\s]+)`)

func (e *env) login(code string, sameState bool) (*loginStep, error) {
	var start loginStep
	if err := e.provision("POST", "/v3/login/start/browser", map[string]any{}, &start); err != nil {
		return nil, err
	}
	state := "someone-else"
	if m := reState.FindStringSubmatch(start.Instructions); m != nil && sameState {
		state = m[1]
	}
	var done loginStep
	err := e.provision("POST", "/v3/login/step/"+start.LoginID+"/"+start.StepID+"/user_input",
		map[string]string{"redirect_url": "http://localhost/?code=" + code + "&state=" + state + "&session_state=x"}, &done)
	return &done, err
}
