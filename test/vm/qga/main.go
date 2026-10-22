// Command qga runs a shell command in a VM through the QEMU guest agent and
// prints its output: the VM test checks an installed system with it, so the
// system needs no test-only login. It waits up to -wait for the agent, which
// starts once the guest has booted, and exits with the command's status.
//
//	go run ./test/vm/qga -socket qga.sock 'arcus doctor'
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

type response struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Desc string `json:"desc"`
	} `json:"error"`
}

type agent struct {
	conn net.Conn
	r    *bufio.Reader
}

// call sends one command and reads its response, skipping stale lines.
func (a *agent) call(command string, args any, timeout time.Duration) (json.RawMessage, error) {
	msg, err := json.Marshal(map[string]any{"execute": command, "arguments": args})
	if err != nil {
		return nil, err
	}
	a.conn.SetDeadline(time.Now().Add(timeout))
	if _, err := a.conn.Write(append(msg, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := a.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp response
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		if resp.Error != nil {
			return nil, errors.New(resp.Error.Desc)
		}
		return resp.Return, nil
	}
}

// connect waits until the agent answers a guest-sync.
func connect(socket string, wait time.Duration) (*agent, error) {
	deadline := time.Now().Add(wait)
	for id := 1; ; id++ {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			a := &agent{conn: conn, r: bufio.NewReader(conn)}
			ret, err := a.call("guest-sync", map[string]int{"id": id}, 5*time.Second)
			if err == nil && strings.TrimSpace(string(ret)) == fmt.Sprint(id) {
				return a, nil
			}
			conn.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no guest agent on %s after %s", socket, wait)
		}
		time.Sleep(2 * time.Second)
	}
}

func run() (int, error) {
	socket := flag.String("socket", "", "the guest agent's socket")
	wait := flag.Duration("wait", 3*time.Minute, "how long to wait for the agent")
	flag.Parse()
	if *socket == "" || flag.NArg() != 1 {
		return 2, errors.New("usage: qga -socket <path> [-wait 3m] '<shell command>'")
	}
	a, err := connect(*socket, *wait)
	if err != nil {
		return 1, err
	}
	defer a.conn.Close()

	ret, err := a.call("guest-exec", map[string]any{
		"path": "/bin/sh", "arg": []string{"-c", flag.Arg(0)}, "capture-output": true,
	}, 10*time.Second)
	if err != nil {
		return 1, err
	}
	var started struct{ PID int }
	if err := json.Unmarshal(ret, &started); err != nil {
		return 1, err
	}
	for {
		ret, err := a.call("guest-exec-status", map[string]int{"pid": started.PID}, 10*time.Second)
		if err != nil {
			return 1, err
		}
		var status struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			Out      string `json:"out-data"`
			Err      string `json:"err-data"`
		}
		if err := json.Unmarshal(ret, &status); err != nil {
			return 1, err
		}
		if status.Exited {
			out, _ := base64.StdEncoding.DecodeString(status.Out)
			errOut, _ := base64.StdEncoding.DecodeString(status.Err)
			os.Stdout.Write(out)
			os.Stderr.Write(errOut)
			return status.ExitCode, nil
		}
		time.Sleep(time.Second)
	}
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "qga:", err)
	}
	os.Exit(code)
}
