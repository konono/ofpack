package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MCP runs over stdio. The CLI is executed as a child process so its ordinary
// progress messages cannot corrupt the JSON-RPC stream on this process's stdout.
func cmdMCP(args []string) error {
	if len(args) != 1 || args[0] != "serve" {
		return fmt.Errorf("usage: ofpack mcp serve")
	}
	return serveMCP(os.Stdin, os.Stdout, runMCPCLI)
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpRunner func(dir string, args []string) (string, error)

func serveMCP(in io.Reader, out io.Writer, run mcpRunner) error {
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	enc := json.NewEncoder(out)
	initialized := false
	for scan.Scan() {
		var req mcpRequest
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			if err := enc.Encode(mcpError(nil, -32700, "invalid JSON")); err != nil {
				return err
			}
			continue
		}
		// Notifications have no id and never receive a response.
		if len(req.ID) == 0 {
			continue
		}
		var response any
		switch {
		case req.JSONRPC != "2.0":
			response = mcpError(req.ID, -32600, "expected JSON-RPC 2.0")
		case req.Method == "initialize":
			initialized = true
			response = mcpResult(req.ID, map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "ofpack", "version": version},
			})
		case !initialized:
			response = mcpError(req.ID, -32000, "initialize the MCP connection first")
		case req.Method == "ping":
			response = mcpResult(req.ID, map[string]any{})
		case req.Method == "tools/list":
			response = mcpResult(req.ID, map[string]any{"tools": mcpTools()})
		case req.Method == "resources/list":
			response = mcpResult(req.ID, map[string]any{"resources": []any{}})
		case req.Method == "resources/templates/list":
			response = mcpResult(req.ID, map[string]any{"resourceTemplates": []any{}})
		case req.Method == "prompts/list":
			response = mcpResult(req.ID, map[string]any{"prompts": []any{}})
		case req.Method == "tools/call":
			var call struct {
				Name      string                     `json:"name"`
				Arguments map[string]json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &call); err != nil {
				response = mcpError(req.ID, -32602, "invalid tool parameters")
			} else {
				response = mcpResult(req.ID, callMCPTool(call.Name, call.Arguments, run))
			}
		default:
			response = mcpError(req.ID, -32601, "method not found")
		}
		if err := enc.Encode(response); err != nil {
			return err
		}
	}
	return scan.Err()
}

func mcpResult(id json.RawMessage, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id json.RawMessage, code int, message string) any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func mcpTools() []any {
	stringProp := func(description string) any { return map[string]string{"type": "string", "description": description} }
	boolProp := func(description string) any { return map[string]string{"type": "boolean", "description": description} }
	schema := func(required []string, props map[string]any) any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	return []any{
		map[string]any{"name": "pack", "description": "Create an ofpack archive from a project directory. May use Podman and the network when targeting another platform.", "inputSchema": schema([]string{"project_dir"}, map[string]any{
			"project_dir":    stringProp("Absolute path to the project root."),
			"output_archive": stringProp("Absolute output path. Defaults to PROJECT_DIR/PROJECT_NAME.ofpack.tar.gz."),
			"target":         stringProp("Optional target, e.g. linux/amd64."),
			"uv_project":     boolProp("Pack a Python project and its wheel closure."),
			"python":         stringProp("Python X.Y for uv_project."),
			"packages":       boolProp("Also include packages.txt."),
			"package_base":   stringProp("APT base image for packages; needs target."),
			"package_repos":  map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "DNF repository IDs for packages."},
			"dry_run":        boolProp("Show the plan without writing an archive."),
		})},
		map[string]any{"name": "unpack", "description": "Safely unpack an archive. Does not install OS packages or restore a Python .venv.", "inputSchema": schema([]string{"archive"}, map[string]any{
			"archive":       stringProp("Absolute path to the archive."),
			"dest":          stringProp("Absolute destination path; otherwise uses the archive's home-relative project path."),
			"force":         boolProp("Replace conflicting files with rollback on failure."),
			"skip_existing": boolProp("Keep conflicting files."),
			"dry_run":       boolProp("Preview without writing."),
		})},
		map[string]any{"name": "inspect", "description": "Read an archive's manifest and summarize its contents.", "inputSchema": schema([]string{"archive"}, map[string]any{
			"archive": stringProp("Absolute path to the archive."),
		})},
	}
}

func callMCPTool(name string, a map[string]json.RawMessage, run mcpRunner) any {
	result := func(msg string, failed bool) any {
		if msg == "" {
			msg = "Done."
		}
		return map[string]any{"content": []any{map[string]string{"type": "text", "text": msg}}, "isError": failed}
	}
	allowed := map[string]map[string]bool{
		"pack":    {"project_dir": true, "output_archive": true, "target": true, "uv_project": true, "python": true, "packages": true, "package_base": true, "package_repos": true, "dry_run": true},
		"unpack":  {"archive": true, "dest": true, "force": true, "skip_existing": true, "dry_run": true},
		"inspect": {"archive": true},
	}
	fields, ok := allowed[name]
	if !ok {
		return result("unknown tool: "+name, true)
	}
	for key := range a {
		if !fields[key] {
			return result("unknown argument: "+key, true)
		}
	}
	str := func(key string) (string, error) {
		v, ok := a[key]
		if !ok {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return s, nil
	}
	boolean := func(key string) (bool, error) {
		v, ok := a[key]
		if !ok {
			return false, nil
		}
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return false, fmt.Errorf("%s must be a boolean", key)
		}
		return b, nil
	}
	requiredPath := func(key string) (string, error) {
		s, err := str(key)
		if err != nil {
			return "", err
		}
		if s == "" || !filepath.IsAbs(s) {
			return "", fmt.Errorf("%s must be an absolute path", key)
		}
		return s, nil
	}
	optionalPath := func(key string) (string, error) {
		s, err := str(key)
		if err != nil {
			return "", err
		}
		if s != "" && !filepath.IsAbs(s) {
			return "", fmt.Errorf("%s must be an absolute path", key)
		}
		return s, nil
	}
	var args []string
	dir := ""
	switch name {
	case "pack":
		project, err := requiredPath("project_dir")
		if err != nil {
			return result(err.Error(), true)
		}
		dir = project
		args = []string{"pack", "-C", project}
		if s, err := optionalPath("output_archive"); err != nil {
			return result(err.Error(), true)
		} else if s != "" {
			args = append(args, "-o", s)
		} else {
			args = append(args, "-o", filepath.Join(project, filepath.Base(project)+".ofpack.tar.gz"))
		}
		for _, key := range []string{"target", "python", "package_base"} {
			if s, err := str(key); err != nil {
				return result(err.Error(), true)
			} else if s != "" {
				args = append(args, "--"+strings.ReplaceAll(key, "_", "-"), s)
			}
		}
		for _, key := range []string{"uv_project", "packages", "dry_run"} {
			if b, err := boolean(key); err != nil {
				return result(err.Error(), true)
			} else if b {
				args = append(args, "--"+strings.ReplaceAll(key, "_", "-"))
			}
		}
		if v, ok := a["package_repos"]; ok {
			var repos []string
			if err := json.Unmarshal(v, &repos); err != nil {
				return result("package_repos must be an array of strings", true)
			}
			for _, repo := range repos {
				args = append(args, "--package-repo", repo)
			}
		}
	case "unpack":
		archive, err := requiredPath("archive")
		if err != nil {
			return result(err.Error(), true)
		}
		args = []string{"unpack", "-f", archive}
		if s, err := optionalPath("dest"); err != nil {
			return result(err.Error(), true)
		} else if s != "" {
			args = append(args, "--dest", s)
		}
		for _, key := range []string{"force", "skip_existing", "dry_run"} {
			if b, err := boolean(key); err != nil {
				return result(err.Error(), true)
			} else if b {
				args = append(args, "--"+strings.ReplaceAll(key, "_", "-"))
			}
		}
	case "inspect":
		archive, err := requiredPath("archive")
		if err != nil {
			return result(err.Error(), true)
		}
		args = []string{"inspect", "-f", archive}
	}
	output, err := run(dir, args)
	if err != nil {
		return result(output+"\n"+err.Error(), true)
	}
	return result(output, false)
}

type cappedOutput struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 65536 {
		keep := 65536 - b.Len()
		if keep > n {
			keep = n
		}
		_, _ = b.Buffer.Write(p[:keep])
		if keep < n {
			b.truncated = true
		}
	} else if n > 0 {
		b.truncated = true
	}
	return n, nil
}

func runMCPCLI(dir string, args []string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(exe, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var output cappedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	msg := strings.TrimSpace(output.String())
	if output.truncated {
		msg += "\n[output truncated at 64 KiB]"
	}
	return msg, err
}
