package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodeExecGlobGrepReadEditAndDryRun(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "src", "app.js"), []byte("const a = 1;\n// TODO fix bug\nconst b = 2;\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Glob + Grep + Read + DryRun Edit
	res := runCodeExec(ExecRequest{
		Cwd:    tmp,
		DryRun: true,
		Code: `
			var files = fs.glob("**/*.js");
			var hits = fs.grep("TODO", "**/*.js");
			var editRes = fs.edit("src/app.js", "const a = 1;", "const a = 42;");
			print("files=" + files.join(","));
			print("hits=" + hits.length + " line=" + hits[0].line);
			({ changed: editRes.changed, occurrences: editRes.occurrences });
		`,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Stdout, "files=src/app.js") || !strings.Contains(res.Stdout, "hits=1 line=2") {
		t.Fatalf("unexpected stdout: %q", res.Stdout)
	}
	if len(res.Diffs) != 1 || !strings.Contains(res.Diffs[0], "+const a = 42;") {
		t.Fatalf("expected dryRun diff, got: %#v", res.Diffs)
	}
	// Verify disk untouched on dryRun
	disk, _ := os.ReadFile(filepath.Join(tmp, "src", "app.js"))
	if !strings.Contains(string(disk), "const a = 1;") {
		t.Fatalf("dryRun modified disk!")
	}

	// 2. Real edit (defaults to diffMode="stat")
	res2 := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("src/app.js", "const a = 1;", "const a = 99;");`,
	})
	if res2.Error != "" {
		t.Fatalf("unexpected error: %s", res2.Error)
	}
	if len(res2.Diffs) != 1 || !strings.Contains(res2.Diffs[0], "src/app.js (+1/-1 lines)") {
		t.Fatalf("expected stat diff by default, got: %#v", res2.Diffs)
	}
	disk2, _ := os.ReadFile(filepath.Join(tmp, "src", "app.js"))
	if !strings.Contains(string(disk2), "const a = 99;") {
		t.Fatalf("real edit did not persist: %s", string(disk2))
	}

	// 3. diffMode="none" omits diffs
	res3 := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "none",
		Code:     `fs.edit("src/app.js", "const a = 99;", "const a = 100;");`,
	})
	if res3.Error != "" || len(res3.Diffs) != 0 {
		t.Fatalf("expected no diffs with diffMode=none, got err=%q diffs=%#v", res3.Error, res3.Diffs)
	}
}

func TestSandboxGuards(t *testing.T) {
	tmp := t.TempDir()

	// 1. Path escape blocked
	res := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.read("../../etc/passwd")`,
	})
	if !strings.Contains(res.Error, "escapes workspace root") {
		t.Fatalf("expected path escape error, got: %+v", res)
	}

	// 2. allowShell=false blocks sh.run
	allow := false
	res2 := runCodeExec(ExecRequest{
		Cwd:        tmp,
		AllowShell: &allow,
		Code:       `sh.run("echo hi")`,
	})
	if !strings.Contains(res2.Error, "sh.run is disabled") {
		t.Fatalf("expected shell disabled error, got: %+v", res2)
	}

	// 3. Timeout interrupts infinite loop
	res3 := runCodeExec(ExecRequest{
		Cwd:       tmp,
		TimeoutMs: 120,
		Code:      `while (true) {}`,
	})
	if !strings.Contains(res3.Error, "timeout") {
		t.Fatalf("expected timeout error, got: %+v", res3)
	}

	// 4. Stdout cap truncates cleanly
	res4 := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 64,
		Code:       `for (var i = 0; i < 100; i++) print("0123456789");`,
	})
	if !res4.Truncated || len(res4.Stdout) > 64 {
		t.Fatalf("expected truncated stdout <= 64 bytes, got len=%d truncated=%v", len(res4.Stdout), res4.Truncated)
	}
}

func TestGlobBraceExpansion(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"**/*.{js,mjs,html,css}", "game/src/main.js", true},
		{"**/*.{js,mjs,html,css}", "serve.mjs", true},
		{"**/*.{js,mjs,html,css}", "game/index.html", true},
		{"**/*.{js,mjs,html,css}", "README.md", false},
		{"{worker,game}/**/*.js", "worker/room-core.js", true},
		{"{worker,game}/**/*.js", "test/foo.js", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pat, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pat, tc.path, got, tc.want)
		}
	}
}

func TestMCPStdioHandshakeAndCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"code_exec","arguments":{"code":"print('hello mcp'); 2 + 2"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := serveStdio(strings.NewReader(input), &out); err != nil {
		t.Fatalf("serveStdio error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d: %s", len(lines), out.String())
	}
	var resp3 map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &resp3); err != nil {
		t.Fatalf("invalid json resp3: %v", err)
	}
	resObj := resp3["result"].(map[string]any)
	if resObj["isError"] == true {
		t.Fatalf("expected non-error tool call: %s", lines[2])
	}
	if !strings.Contains(lines[2], "hello mcp") {
		t.Fatalf("expected hello mcp in output: %s", lines[2])
	}
}
