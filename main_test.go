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

	// 2. Real edit (defaults to diffMode="auto" -> compact ±3-line unified diff for small edits!)
	res2 := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("src/app.js", "const a = 1;", "const a = 99;");`,
	})
	if res2.Error != "" {
		t.Fatalf("unexpected error: %s", res2.Error)
	}
	if len(res2.Diffs) != 1 || !strings.Contains(res2.Diffs[0], "+const a = 99;") || !strings.Contains(res2.Diffs[0], " // TODO fix bug") {
		t.Fatalf("expected auto compact diff with surrounding context, got: %#v", res2.Diffs)
	}
	disk2, _ := os.ReadFile(filepath.Join(tmp, "src", "app.js"))
	if !strings.Contains(string(disk2), "const a = 99;") {
		t.Fatalf("real edit did not persist: %s", string(disk2))
	}

	// 3. Explicit diffMode="stat" returns one-line summary
	resStat := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "stat",
		Code:     `fs.edit("src/app.js", "const a = 99;", "const a = 98;");`,
	})
	if resStat.Error != "" || len(resStat.Diffs) != 1 || resStat.Diffs[0] != "src/app.js (+1/-1 lines)" {
		t.Fatalf("expected stat diff, got err=%q diffs=%#v", resStat.Error, resStat.Diffs)
	}

	// 4. diffMode="none" omits diffs
	res3 := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "none",
		Code:     `fs.edit("src/app.js", "const a = 98;", "const a = 100;");`,
	})
	if res3.Error != "" || len(res3.Diffs) != 0 {
		t.Fatalf("expected no diffs with diffMode=none, got err=%q diffs=%#v", res3.Error, res3.Diffs)
	}
}

func TestEditStrictnessMultiEditAndPerHunkDiff(t *testing.T) {
	tmp := t.TempDir()
	// Build a 160-line file with two identical "if (flag) run();" lines far apart
	var lines []string
	for i := 1; i <= 160; i++ {
		switch i {
		case 5, 155:
			lines = append(lines, "if (flag) run();")
		default:
			lines = append(lines, "const line_"+string(rune('a'+(i%26)))+" = "+strings.Repeat("x", 1)+";")
		}
	}
	orig := strings.Join(lines, "\n") + "\n"
	fp := filepath.Join(tmp, "big.js")
	if err := os.WriteFile(fp, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Unmatched target throws by default with target preview
	res := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("big.js", "NO_SUCH_TEXT", "X");`,
	})
	if !strings.Contains(res.Error, `"NO_SUCH_TEXT" not found`) {
		t.Fatalf("expected target preview + not found error, got: %+v", res)
	}

	// 2. Unmatched target succeeds silently with { optional: true }
	resOpt := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("big.js", "NO_SUCH_TEXT", "X", { optional: true });`,
	})
	if resOpt.Error != "" {
		t.Fatalf("expected optional edit to succeed, got: %s", resOpt.Error)
	}

	// 3. Ambiguous (>1 match) throws by default
	resAmb := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("big.js", "if (flag) run();", "if (flag) runFast();");`,
	})
	if !strings.Contains(resAmb.Error, "matched 2 times") {
		t.Fatalf("expected ambiguity error, got: %+v", resAmb)
	}

	// 4. Ambiguous succeeds with { replaceAll: true } AND reports exact per-hunk (+2/-2 lines), NOT (+151/-151)!
	resAll := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "stat",
		Code:     `fs.edit("big.js", "if (flag) run();", "if (flag) runFast();", { replaceAll: true });`,
	})
	if resAll.Error != "" {
		t.Fatalf("expected replaceAll to succeed, got: %s", resAll.Error)
	}
	if len(resAll.Diffs) != 1 || resAll.Diffs[0] != "big.js (+2/-2 lines)" {
		t.Fatalf("expected exact per-hunk stat 'big.js (+2/-2 lines)', got: %#v", resAll.Diffs)
	}

	// 5. Atomic multi-edit: fails cleanly on 2nd edit without modifying disk
	beforeAtomic, _ := os.ReadFile(fp)
	resFailMulti := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `fs.edit("big.js", [
			{ target: "if (flag) runFast();", replacement: "if (flag) runV2();", replaceAll: true },
			{ target: "DOES_NOT_EXIST", replacement: "OOPS" }
		]);`,
	})
	if !strings.Contains(resFailMulti.Error, `edit[1] target "DOES_NOT_EXIST" not found`) {
		t.Fatalf("expected edit[1] error with target preview, got: %+v", resFailMulti)
	}
	afterFailAtomic, _ := os.ReadFile(fp)
	if string(afterFailAtomic) != string(beforeAtomic) {
		t.Fatalf("failed atomic multi-edit modified disk!")
	}

	// 6. Atomic multi-edit succeeds when all match
	resOkMulti := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "compact",
		Code: `fs.edit("big.js", [
			{ target: "if (flag) runFast();", replacement: "if (flag) runV2();", replaceAll: true },
			{ target: "MISSING_OPT", replacement: "NOOP", optional: true }
		]);`,
	})
	if resOkMulti.Error != "" {
		t.Fatalf("expected atomic multi-edit to succeed, got: %s", resOkMulti.Error)
	}
	if len(resOkMulti.Diffs) != 1 || strings.Count(resOkMulti.Diffs[0], "@@ -") != 2 {
		t.Fatalf("expected 2 separate unified hunks in compact diff, got: %#v", resOkMulti.Diffs)
	}
}

func TestGrepValidationAndReadFn(t *testing.T) {
	tmp := t.TempDir()
	js := `// header
function syncCoopLobby(lobby, myPeerId) {
  const isHost = lobby.hostId === myPeerId;
  if (isHost) {
    return "HOST";
  }
  return "GUEST";
}
// footer
`
	py := `def helper(x):
    if x > 0:
        return x * 2
    return 0

def next_fn():
    pass
`
	_ = os.WriteFile(filepath.Join(tmp, "ui.js"), []byte(js), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "util.py"), []byte(py), 0644)

	// 1. fs.grep accepts JS RegExp literals with /i flag
	resRe := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.grep(/ishost/i, "**/*.js");`,
	})
	if resRe.Error != "" {
		t.Fatalf("expected RegExp grep to work, got: %s", resRe.Error)
	}
	raw, _ := json.Marshal(resRe.Result)
	if !strings.Contains(string(raw), `"line":3`) || !strings.Contains(string(raw), `"line":4`) {
		t.Fatalf("expected case-insensitive hits on lines 3 & 4, got: %s", string(raw))
	}

	// 2. fs.grep detects swapped (filePath, regex) arguments and throws a helpful error
	resSwap := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.grep("ui.js", "isHost");`,
	})
	if !strings.Contains(resSwap.Error, "arguments look swapped") {
		t.Fatalf("expected swapped-arg error, got: %+v", resSwap)
	}

	// 3. fs.grep rejects non-string/non-RegExp first argument
	resBad := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.grep(123, "*.js");`,
	})
	if !strings.Contains(resBad.Error, "must be a string or RegExp") {
		t.Fatalf("expected type error on numeric regex, got: %+v", resBad)
	}

	// 4. fs.readFn extracts full JS function with line numbers
	resFn := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `var r = fs.readFn("ui.js", "syncCoopLobby"); print(r.text); r;`,
	})
	if resFn.Error != "" {
		t.Fatalf("readFn JS failed: %s", resFn.Error)
	}
	if !strings.Contains(resFn.Stdout, "   2: function syncCoopLobby") || !strings.Contains(resFn.Stdout, "   8: }") {
		t.Fatalf("expected full JS function body, got:\n%s", resFn.Stdout)
	}

	// 5. fs.readFn extracts full Python function
	resPy := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `var r = fs.readFn("util.py", "helper"); print(r.text); r;`,
	})
	if resPy.Error != "" {
		t.Fatalf("readFn Python failed: %s", resPy.Error)
	}
	if !strings.Contains(resPy.Stdout, "   1: def helper(x):") || !strings.Contains(resPy.Stdout, "   4:     return 0") {
		t.Fatalf("expected full Python function body, got:\n%s", resPy.Stdout)
	}

	// 6. fs.readFn returns ranked suggestions when symbol is slightly misnamed
	_ = os.WriteFile(filepath.Join(tmp, "room.js"), []byte(`
function createPlayerForPeer(peer) { return peer; }
function startMatch() {}
function broadcastLobby() {}
`), 0644)
	resMiss := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.readFn("room.js", "createPlayerEntity");`,
	})
	if resMiss.Error != "" {
		t.Fatalf("readFn miss unexpected error: %s", resMiss.Error)
	}
	missJSON, _ := json.Marshal(resMiss.Result)
	if !strings.Contains(string(missJSON), `"found":false`) || !strings.Contains(string(missJSON), `"createPlayerForPeer"`) {
		t.Fatalf("expected suggestions containing createPlayerForPeer, got: %s", string(missJSON))
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

func TestCrossFileAtomicRollbackAndCommit(t *testing.T) {
	tmp := t.TempDir()
	aPath := filepath.Join(tmp, "a.js")
	bPath := filepath.Join(tmp, "b.js")
	_ = os.WriteFile(aPath, []byte("const a = 1;\n"), 0644)
	_ = os.WriteFile(bPath, []byte("const b = 2;\n"), 0644)

	// 1. File #1 succeeds, file #2 fails -> atomic rollback leaves a.js untouched on disk!
	resFail := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			fs.edit("a.js", "const a = 1;", "const a = 999;");
			fs.edit("b.js", "DOES_NOT_MATCH", "const b = 888;");
		`,
	})
	if resFail.Error == "" || !resFail.RolledBack {
		t.Fatalf("expected error + RolledBack=true, got: %+v", resFail)
	}
	if len(resFail.EditedFiles) != 0 {
		t.Fatalf("expected 0 editedFiles after rollback, got: %#v", resFail.EditedFiles)
	}
	diskA, _ := os.ReadFile(aPath)
	if string(diskA) != "const a = 1;\n" {
		t.Fatalf("rollback failed: a.js was modified on disk: %q", string(diskA))
	}

	// 2. Same-script read sees staged edit + sh.run auto-commits so tests see new files
	resOk := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			fs.edit("a.js", "const a = 1;", "const a = 42;");
			var staged = fs.read("a.js");
			if (staged.indexOf("42") === -1) throw new Error("read did not see staged edit");
			fs.edit("b.js", "const b = 2;", "const b = 84;");
			var shRes = sh.run("cat a.js");
			print("sh=" + shRes.stdout.trim());
		`,
	})
	if resOk.Error != "" || !strings.Contains(resOk.Stdout, "sh=const a = 42;") {
		t.Fatalf("expected atomic commit + sh.run visibility, got: %+v", resOk)
	}
	diskB, _ := os.ReadFile(bPath)
	if string(diskB) != "const b = 84;\n" {
		t.Fatalf("b.js did not persist: %q", string(diskB))
	}
}

func TestCodeReadToolAndEditLines(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "tpl.js")
	_ = os.WriteFile(fp, []byte("const title = 'hi';\nconst msg = `hello ${name}\\nworld`;\nconst end = true;\n"), 0644)

	// 1. code_read via MCP RPC allows read but blocks writes & shell
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_read","arguments":{"cwd":` + mustJSON(tmp) + `,"code":"print(fs.readLines('tpl.js', 1, 2));"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"code_read","arguments":{"cwd":` + mustJSON(tmp) + `,"code":"fs.write('hack.txt', 'no');"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"code_read","arguments":{"cwd":` + mustJSON(tmp) + `,"code":"sh.run('echo nope');"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := serveStdio(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "const title") || strings.Contains(lines[0], `"isError":true`) {
		t.Fatalf("expected code_read readLines to succeed: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"isError":true`) {
		t.Fatalf("expected code_read fs.write to fail: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) {
		t.Fatalf("expected code_read sh.run to fail: %s", lines[2])
	}

	// 2. fs.editLines rejects wrong expectStart
	resBadExpect := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.editLines("tpl.js", 2, 2, "const msg = 'clean';", { expectStart: "WRONG_PREFIX" });`,
	})
	if !strings.Contains(resBadExpect.Error, "does not contain expectStart") {
		t.Fatalf("expected expectStart mismatch error, got: %+v", resBadExpect)
	}

	// 3. fs.editLines replaces line 2 cleanly without escaping template literal target
	resLine := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.editLines("tpl.js", 2, 2, "const msg = 'clean';", { expectStart: "const msg" });`,
	})
	if resLine.Error != "" {
		t.Fatalf("fs.editLines failed: %s", resLine.Error)
	}
	disk, _ := os.ReadFile(fp)
	if string(disk) != "const title = 'hi';\nconst msg = 'clean';\nconst end = true;\n" {
		t.Fatalf("unexpected tpl.js content: %q", string(disk))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
