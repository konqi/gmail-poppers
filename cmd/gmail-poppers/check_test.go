package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `accounts:
  a@gmail.com:
    sources:
      shared: {type: pop3, server: localhost, user: u, password: p}
  b@gmail.com:
    sources:
      shared: {type: imap, server: localhost, user: u, password: p}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--mode", "source", "--config", path, "shared"}, "ambiguous"},
		{[]string{"--mode", "source", "--config", path, "a@gmail.com/missing"}, "unknown source"},
		{[]string{"--mode", "target", "--config", path, "missing@gmail.com"}, "unknown target"},
	} {
		if err := runTest(test.args, io.Discard); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v: unexpected error %v", test.args, err)
		}
	}
	if err := runTest([]string{"--help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--mode", "unknown", "name"}, {"--mode", "source", "--timeout", "0s", "name"}} {
		if err := runTest(args, io.Discard); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestSourceCommand(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan []string, 1)
		go func() {
			connection, err := listener.Accept()
			if err != nil {
				done <- nil
				return
			}
			defer connection.Close()
			if stalled {
				io.Copy(io.Discard, connection)
				done <- nil
				return
			}
			fmt.Fprint(connection, "+OK ready\r\n")
			reader := bufio.NewReader(connection)
			var commands []string
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					break
				}
				command := strings.Fields(line)[0]
				commands = append(commands, command)
				if command == "UIDL" {
					fmt.Fprint(connection, "+OK\r\n1 uid-one\r\n.\r\n")
				} else {
					fmt.Fprint(connection, "+OK\r\n")
				}
				if command == "QUIT" {
					break
				}
			}
			done <- commands
		}()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		data := fmt.Sprintf(`accounts:
  a@gmail.com:
    sources:
      local: {type: pop3, server: 127.0.0.1, port: %d, tls: none, user: u, password: p, delete_after_import: true, initial_sync: all}
`, listener.Addr().(*net.TCPAddr).Port)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
		var output bytes.Buffer
		err = runTest([]string{"--mode", "source", "--config", path, "--timeout", "1s", "a@gmail.com/local"}, &output)
		listener.Close()
		commands := <-done
		if stalled {
			if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
				t.Fatalf("unexpected timeout result: %v", err)
			}
		} else {
			if err != nil || !strings.Contains(output.String(), "OK source a@gmail.com/local") {
				t.Fatalf("unexpected result: %v %s", err, output.String())
			}
			if strings.Join(commands, ",") != "USER,PASS,UIDL,QUIT" {
				t.Fatalf("unexpected commands: %v", commands)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
			t.Fatal("connection test must not create data or state directories")
		}
	}
}

func TestTargetCommandMissingTokenDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`accounts:
  me@gmail.com:
    sources:
      local: {type: pop3, server: localhost, user: u, password: p}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_CLIENT_ID", "id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "secret")
	tokens := filepath.Join(dir, "tokens")
	err := runTest([]string{"--mode", "target", "--config", path, "--client-secret", "", "--token-dir", tokens, "me@gmail.com"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "token store:") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Fatal("check must not create a token directory")
	}
}
