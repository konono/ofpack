package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMCPHandshakeAndTools(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pack","arguments":{"project_dir":"/work/app","target":"linux/amd64","packages":true,"package_repos":["baseos","appstream"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"unpack","arguments":{"archive":"/tmp/a.tar.gz","dest":"/work/app/.ofpack","dry_run":true}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"inspect","arguments":{"archive":"/tmp/a.tar.gz"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	var calls [][]string
	run := func(dir string, args []string) (string, error) {
		calls = append(calls, append([]string{dir}, args...))
		return "CLI output", nil
	}
	if err := serveMCP(strings.NewReader(input), &output, run); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("responses = %d: %s", len(lines), output.String())
	}
	var init struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &init); err != nil {
		t.Fatal(err)
	}
	if init.Result.ProtocolVersion != "2025-11-25" || init.Result.ServerInfo.Name != "ofpack" {
		t.Fatalf("bad init: %s", lines[0])
	}
	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Result.Tools {
		names = append(names, tool.Name)
	}
	if !reflect.DeepEqual(names, []string{"pack", "unpack", "inspect"}) {
		t.Fatalf("tools: %v", names)
	}
	if len(calls) != 3 {
		t.Fatalf("calls: %v", calls)
	}
	if !reflect.DeepEqual(calls[0], []string{"/work/app", "pack", "-C", "/work/app", "-o", "/work/app/app.ofpack.tar.gz", "--target", "linux/amd64", "--packages", "--package-repo", "baseos", "--package-repo", "appstream"}) {
		t.Fatalf("pack call: %v", calls[0])
	}
	if !reflect.DeepEqual(calls[1], []string{"", "unpack", "-f", "/tmp/a.tar.gz", "--dest", "/work/app/.ofpack", "--dry-run"}) {
		t.Fatalf("unpack call: %v", calls[1])
	}
	if !reflect.DeepEqual(calls[2], []string{"", "inspect", "-f", "/tmp/a.tar.gz"}) {
		t.Fatalf("inspect call: %v", calls[2])
	}
	for _, line := range lines[2:] {
		if !strings.Contains(line, `"text":"CLI output"`) || !strings.Contains(line, `"isError":false`) {
			t.Fatalf("bad tool result: %s", line)
		}
	}
}

func TestMCPRejectsBadArgumentsAndReturnsCLIErrors(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"inspect","arguments":{"archive":"relative.tar.gz"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"inspect","arguments":{"archive":"/tmp/a.tar.gz"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	count := 0
	if err := serveMCP(strings.NewReader(input), &output, func(_ string, _ []string) (string, error) {
		count++
		return "archive invalid", errors.New("exit status 1")
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CLI calls = %d", count)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if !strings.Contains(lines[1], `"isError":true`) || !strings.Contains(lines[1], "absolute path") {
		t.Fatalf("bad validation: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "archive invalid") {
		t.Fatalf("bad CLI error: %s", lines[2])
	}
}

func TestMCPAnswersEmptyOptionalLists(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/templates/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"prompts/list"}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := serveMCP(strings.NewReader(input), &output, func(string, []string) (string, error) { t.Fatal("unexpected CLI call"); return "", nil }); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses: %s", output.String())
	}
	for i, field := range []string{`"resources":[]`, `"resourceTemplates":[]`, `"prompts":[]`} {
		if !strings.Contains(lines[i+1], field) {
			t.Fatalf("%s missing in %s", field, lines[i+1])
		}
	}
}
