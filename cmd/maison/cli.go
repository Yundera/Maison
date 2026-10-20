package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yundera/maison/internal/config"
)

// The maison CLI: how a deployment manages this box's settings and secrets.
//
// Meant to be run inside the container, by a host script:
//
//	docker exec -i maison-app maison settings set smtp --owner mesh <<<'{"host":"smtp","port":587}'
//
// Every command is a request to the running server over its admin socket
// (internal/server/admin.go) — never a direct edit of the state files, which the
// server holds in memory and would overwrite. Any failure exits non-zero with the
// server's reason, so a self-check log shows exactly what an image refused.

const cliUsage = `usage: maison <command>

  settings get                         print the settings (secrets masked)
  settings set <name> --owner <owner>  manage a setting; its value is JSON on stdin
  settings release <name>              hand a managed setting back to the user
  secret set <name>                    store a secret; its value on stdin
  secret rm <name>                     delete a secret
  stores ensure <url>                  add a store source unless already listed
  backup approve <engine>              enable a backup engine the deployment provisioned
  backup status                        print the backup status (as GET /api/backup/status)
  backup init <engine>                 create the engine's repository; the password on stdin
  backup recover <engine>              reattach to the existing repository; its key on stdin
  lint <docker-compose.yml>…           check store compose files as Maison reads them (no server needed)

Manageable settings: domains, feedback, onboarding, smtp.
A setting refers to a secret as the whole string "${secret:<name>}".
A password on stdin is one line; the trailing newline is dropped.
On failure the server's reason goes to stderr, with its code in brackets when it
gave one, e.g. "maison: that key does not open these backups [wrong_key]".
`

// dialRetry is how long a command waits for the socket to appear: a self-check runs
// right after (re)deploying the container, before the server has finished booting.
const dialRetry = 30 * time.Second

// Request timeouts. Most commands answer in milliseconds; init and recover run an
// engine verb to completion before answering, and a recovery pins every snapshot in
// the space. Kept just above the server's own bounds for those verbs.
const (
	callTimeout    = 60 * time.Second
	initTimeout    = 4 * time.Minute
	recoverTimeout = 16 * time.Minute
)

func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, cliUsage)
		return 0
	}
	if len(args) > 0 && args[0] == "lint" {
		return runLint(args[1:], stdout, stderr) // local: reads files, needs no server
	}
	method, path, body, err := parseCLI(args, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		if errors.Is(err, errUsage) {
			fmt.Fprint(stderr, cliUsage)
			return 2
		}
		return 1
	}
	out, err := adminCall(config.AdminSocketPath(), method, path, body, timeoutFor(path))
	if err != nil {
		fmt.Fprintln(stderr, "maison:", err)
		return 1
	}
	fmt.Fprintln(stdout, strings.TrimRight(string(out), "\n"))
	return 0
}

var errUsage = errors.New("maison: invalid command")

// parseCLI maps a command line onto an admin-socket request.
func parseCLI(args []string, stdin io.Reader) (method, path string, body []byte, err error) {
	usage := func() (string, string, []byte, error) { return "", "", nil, errUsage }
	if len(args) < 2 {
		return usage()
	}
	readStdin := func(what string) ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, fmt.Errorf("maison: %s expected on stdin (use docker exec -i)", what)
		}
		return b, nil
	}
	// readKey is one line from stdin: the password the deployment's tool was handed.
	// Only the line ending is dropped — leading and trailing spaces are the server's
	// to judge, as they are for the dashboard's own form.
	readKey := func() (string, error) {
		b, err := readStdin("the password")
		if err != nil {
			return "", err
		}
		key := strings.TrimRight(string(b), "\r\n")
		if strings.ContainsAny(key, "\r\n") {
			return "", errors.New("maison: the password on stdin must be a single line")
		}
		return key, nil
	}
	switch args[0] + " " + args[1] {
	case "settings get":
		if len(args) != 2 {
			return usage()
		}
		return http.MethodGet, "/settings", nil, nil
	case "settings set":
		if len(args) != 5 || args[3] != "--owner" {
			return usage()
		}
		b, err := readStdin("the setting's JSON value")
		if err != nil {
			return "", "", nil, err
		}
		if !json.Valid(b) {
			return "", "", nil, errors.New("maison: the value on stdin is not valid JSON")
		}
		return http.MethodPut, "/settings/" + url.PathEscape(args[2]) + "?owner=" + url.QueryEscape(args[4]), b, nil
	case "settings release":
		if len(args) != 3 {
			return usage()
		}
		return http.MethodDelete, "/settings/" + url.PathEscape(args[2]), nil, nil
	case "secret set":
		if len(args) != 3 {
			return usage()
		}
		b, err := readStdin("the secret's value")
		if err != nil {
			return "", "", nil, err
		}
		return http.MethodPut, "/secrets/" + url.PathEscape(args[2]), b, nil
	case "secret rm":
		if len(args) != 3 {
			return usage()
		}
		return http.MethodDelete, "/secrets/" + url.PathEscape(args[2]), nil, nil
	case "stores ensure":
		if len(args) != 3 {
			return usage()
		}
		b, _ := json.Marshal(map[string]string{"url": args[2]})
		return http.MethodPost, "/stores/ensure", b, nil
	case "backup approve":
		if len(args) != 3 {
			return usage()
		}
		return http.MethodPost, "/backup/engines/" + url.PathEscape(args[2]) + "/approve", nil, nil
	case "backup status":
		if len(args) != 2 {
			return usage()
		}
		return http.MethodGet, "/backup/status", nil, nil
	case "backup init", "backup recover":
		if len(args) != 3 {
			return usage()
		}
		key, err := readKey()
		if err != nil {
			return "", "", nil, err
		}
		in := map[string]any{"key": key}
		if args[1] == "init" {
			// The deployment's tool only runs this once the owner confirmed they saved
			// the password, on its own page; the server insists on hearing it.
			in["confirmed"] = true
		}
		b, _ := json.Marshal(in)
		return http.MethodPost, "/backup/engines/" + url.PathEscape(args[2]) + "/" + args[1], b, nil
	}
	return usage()
}

// timeoutFor is the request timeout for one admin-socket path.
func timeoutFor(path string) time.Duration {
	switch {
	case strings.HasSuffix(path, "/recover"):
		return recoverTimeout
	case strings.HasSuffix(path, "/init"):
		return initTimeout
	}
	return callTimeout
}

// adminCall sends one request to the admin socket, waiting up to dialRetry for the
// server to come up. A non-2xx answer is an error carrying the server's reason, and its
// code when it gave one.
func adminCall(socket, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				deadline := time.Now().Add(dialRetry)
				for {
					c, err := d.DialContext(ctx, "unix", socket)
					if err == nil || time.Now().After(deadline) {
						if err != nil {
							return nil, fmt.Errorf("cannot reach the dashboard at %s: %w", socket, err)
						}
						return c, nil
					}
					time.Sleep(time.Second)
				}
			},
		},
	}
	req, err := http.NewRequest(method, "http://maison"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			if e.Code != "" {
				return nil, fmt.Errorf("%s [%s]", e.Error, e.Code)
			}
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return out, nil
}
