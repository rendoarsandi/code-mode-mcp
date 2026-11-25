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

	// 1. code_read via MCP RPC allows read, omits sh (typeof sh === 'undefined'), and blocks writes
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_read","arguments":{"cwd":` + mustJSON(tmp) + `,"code":"print(fs.readLines('tpl.js', 1, 2)); typeof sh;"}}}`,
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
	if !strings.Contains(lines[0], "const title") || !strings.Contains(lines[0], "undefined") || strings.Contains(lines[0], `"isError":true`) {
		t.Fatalf("expected code_read readLines + typeof sh === 'undefined': %s", lines[0])
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

func TestSymbolsReadFnAtPlainTextAndTruncation(t *testing.T) {
	tmp := t.TempDir()
	js := `// top-level config
const MAX_CHUNKS = 64;
const VIEW_DIST = 8;

class VoxelWorld {
  constructor(seed) {
    this.seed = seed;
  }

  buildMesh(cx, cz) {
    const verts = [];
    if (cx < 0 && cz > 0) {
      verts.push(cx, cz);
    }
    return verts;
  }
}

function helper(a, b) {
  return a + b;
}
`
	if err := os.WriteFile(filepath.Join(tmp, "voxel.js"), []byte(js), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. fs.symbols extracts class, methods, and functions with line ranges
	resSyms := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.symbols("voxel.js");`,
	})
	if resSyms.Error != "" {
		t.Fatalf("fs.symbols failed: %s", resSyms.Error)
	}
	symsJSON := marshalCompactNoEscape(resSyms.Result)
	for _, want := range []string{`"name":"VoxelWorld"`, `"kind":"class"`, `"name":"constructor"`, `"name":"buildMesh"`, `"name":"helper"`} {
		if !strings.Contains(symsJSON, want) {
			t.Fatalf("expected %s in symbols output, got: %s", want, symsJSON)
		}
	}

	// 2. fs.readFnAt picks innermost enclosing method (buildMesh), NOT outer VoxelWorld class
	resAt := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `var r = fs.readFnAt("voxel.js", 12); print(r.text); r;`,
	})
	if resAt.Error != "" {
		t.Fatalf("fs.readFnAt failed: %s", resAt.Error)
	}
	atJSON := marshalCompactNoEscape(resAt.Result)
	if !strings.Contains(atJSON, `"found":true`) || !strings.Contains(atJSON, `"symbol":"buildMesh"`) {
		t.Fatalf("expected enclosing method buildMesh, got: %s", atJSON)
	}
	if !strings.Contains(resAt.Stdout, "  10:   buildMesh(cx, cz) {") || !strings.Contains(resAt.Stdout, "  16:   }") {
		t.Fatalf("expected full buildMesh body, got:\n%s", resAt.Stdout)
	}

	// 3. fs.readFnAt on top-level line returns found:false + numbered fallback context
	resTop := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.readFnAt("voxel.js", 2);`,
	})
	if resTop.Error != "" {
		t.Fatalf("fs.readFnAt top-level failed: %s", resTop.Error)
	}
	topJSON := marshalCompactNoEscape(resTop.Result)
	if !strings.Contains(topJSON, `"found":false`) || !strings.Contains(topJSON, `MAX_CHUNKS`) {
		t.Fatalf("expected found:false with fallback text for top-level line, got: %s", topJSON)
	}

	// 4. Plain-text MCP formatting avoids JSON/HTML escaping (<, >, &) and reports truncation cleanly
	resTrunc := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 64,
		Code:       `for (var i = 0; i < 20; i++) print("0123456789");`,
	})
	if !resTrunc.Truncated || resTrunc.TotalBytes != 220 {
		t.Fatalf("expected truncated=true totalBytes=220, got truncated=%v totalBytes=%d", resTrunc.Truncated, resTrunc.TotalBytes)
	}
	if !strings.HasSuffix(resTrunc.Stdout, "\n") || len(resTrunc.Stdout) != 55 {
		t.Fatalf("expected clean newline-snapped 55 bytes, got len=%d stdout=%q", len(resTrunc.Stdout), resTrunc.Stdout)
	}
	formatted := formatExecResult(resTrunc)
	if !strings.Contains(formatted, "[truncated: showed 55/220 bytes (165 dropped)") {
		t.Fatalf("expected truncation summary in formatted text, got:\n%s", formatted)
	}

	// 5. UTF-8 rune boundary safety on single-line truncation
	resUTF8 := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 10,
		Code:       `print("éééééééé");`,
	})
	if !resUTF8.Truncated || len(resUTF8.Stdout) > 10 || len(resUTF8.Stdout)%2 != 0 {
		t.Fatalf("expected valid UTF-8 prefix <= 10 bytes, got len=%d stdout=%q", len(resUTF8.Stdout), resUTF8.Stdout)
	}

	// 6. MCP stdio response uses plain text without \u003c / \u003e / \u0026 escaping & includes initialize instructions
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"code_read","arguments":{"cwd":` + mustJSON(tmp) + `,"code":"print('if (a < b && c > d) {}');"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveStdio(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(lines))
	}
	var initResp, callResp map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &initResp)
	_ = json.Unmarshal([]byte(lines[1]), &callResp)
	initRes := initResp["result"].(map[string]any)
	if instr, ok := initRes["instructions"].(string); !ok || !strings.Contains(instr, "code_read") {
		t.Fatalf("expected initialize instructions mentioning code_read, got: %v", initRes["instructions"])
	}
	callRes := callResp["result"].(map[string]any)
	content := callRes["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "if (a < b && c > d) {}") || strings.Contains(content, `\u003c`) {
		t.Fatalf("expected unescaped plain-text stdout, got:\n%s", content)
	}
}

func TestHeadTailTruncationOutputCursorOutBudgetAndResultBound(t *testing.T) {
	tmp := t.TempDir()

	// 1. Head + tail preservation when limit >= 512 keeps BOTH start and trailing SUMMARY line!
	res := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 800,
		Code: `
			print("HEADER_START");
			for (var i = 0; i < 60; i++) {
				print("middle-line-" + i + "-payload-abcdefghij");
			}
			print("FINAL_SUMMARY_OK=42");
		`,
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Truncated || res.OutputId == "" || res.NextOffset <= 0 {
		t.Fatalf("expected truncated + outputId + nextOffset, got: truncated=%v id=%q next=%d", res.Truncated, res.OutputId, res.NextOffset)
	}
	if len(res.Stdout) > 800 {
		t.Fatalf("expected combined head+tail <= 800 bytes, got %d", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "HEADER_START") || !strings.Contains(res.Stdout, "bytes omitted") || !strings.Contains(res.Stdout, "FINAL_SUMMARY_OK=42") {
		t.Fatalf("expected head+tail with omitted marker and trailing summary, got:\n%s", res.Stdout)
	}

	// 2. Continue reading dropped bytes via code_read({ outputId, outputOffset }) without re-running script!
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"code_read","arguments":{"outputId":` + mustJSON(res.OutputId) + `,"outputOffset":` + mustJSON(res.NextOffset) + `,"maxOutputBytes":1000}}}` + "\n"
	var rpcOut bytes.Buffer
	if err := serveStdio(strings.NewReader(input), &rpcOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rpcOut.String(), "middle-line-") || strings.Contains(rpcOut.String(), `"isError":true`) {
		t.Fatalf("expected cached continuation chunk, got: %s", rpcOut.String())
	}

	// 3. out.{limit,used,remaining,truncated}() lets scripts budget their own output
	resBudget := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 200,
		Code: `
			var lim = out.limit();
			print("0123456789");
			var u1 = out.used();
			var r1 = out.remaining();
			var t1 = out.truncated();
			({ lim: lim, u1: u1, r1: r1, t1: t1 });
		`,
	})
	if resBudget.Error != "" {
		t.Fatalf("out budget helper failed: %s", resBudget.Error)
	}
	bj := marshalCompactNoEscape(resBudget.Result)
	if !strings.Contains(bj, `"lim":200`) || !strings.Contains(bj, `"u1":11`) || !strings.Contains(bj, `"r1":189`) || !strings.Contains(bj, `"t1":false`) {
		t.Fatalf("unexpected out budget result: %s", bj)
	}

	// 4. Returning a huge expression directly is bounded in formatExecResult (cannot bypass maxOutputBytes)
	resBig := runCodeExec(ExecRequest{
		Cwd:        tmp,
		MaxOutByte: 1200,
		Code:       `var s = ""; for (var i = 0; i < 400; i++) s += "0123456789"; s;`,
	})
	formatted := formatExecResult(resBig)
	if !strings.Contains(formatted, "[result truncated:") || len(formatted) > 1500 {
		t.Fatalf("expected bounded res.Result in formatExecResult, got len=%d:\n%s", len(formatted), formatted[:min(300, len(formatted))])
	}
}

func TestHardenedReadFnCommentsRegexTSGenericsAndMultiMatch(t *testing.T) {
	tmp := t.TempDir()
	ts := `/*
function fakeInsideComment() {
  return "}";
}
*/
export class Engine {
  public async buildMesh(
    cx: number,
    cz: number,
  ): Promise<string> {
    const re = /\{|\}/g;
    const tpl = ` + "`" + `
      } fake closing brace in template
      ${format({ a: 1 })}
    ` + "`" + `;
    /* } fake closing brace in block comment */
    return tpl.replace(re, "");
  }

  render(): void {
    return;
  }
}

export class Overlay {
  render(): void {
    return;
  }
}
`
	goSrc := `package demo

func Map[T any, U any](
	in []T,
	fn func(T) U,
) []U {
	raw := ` + "`" + `
}
func fakeInRawString() {}
` + "`" + `
	_ = raw
	return nil
}
`
	_ = os.WriteFile(filepath.Join(tmp, "engine.ts"), []byte(ts), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "map.go"), []byte(goSrc), 0644)

	// 1. TS multiline method with return type, regex /\{|\}/, template literal, and block comment braces
	resTS := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `var syms = fs.symbols("engine.ts"); var fn = fs.readFn("engine.ts", "buildMesh"); ({ syms: syms, fn: fn });`,
	})
	if resTS.Error != "" {
		t.Fatalf("TS readFn failed: %s", resTS.Error)
	}
	jTS := marshalCompactNoEscape(resTS.Result)
	if strings.Contains(jTS, "fakeInsideComment") {
		t.Fatalf("should skip symbols inside block comment, got: %s", jTS)
	}
	if !strings.Contains(jTS, `"symbol":"buildMesh"`) || !strings.Contains(jTS, `return tpl.replace(re,`) {
		t.Fatalf("expected full buildMesh body past regex/template/comment braces, got: %s", jTS)
	}

	// 2. Multi-match reporting on duplicate method name "render"
	resMulti := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.readFn("engine.ts", "render");`,
	})
	jMulti := marshalCompactNoEscape(resMulti.Result)
	if !strings.Contains(jMulti, `"matches":[20,26]`) {
		t.Fatalf("expected matches:[20,26] for duplicate render methods, got: %s", jMulti)
	}

	// 3. Go generic function with raw backtick string containing "}" and fake func
	resGo := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `var syms = fs.symbols("map.go"); var fn = fs.readFn("map.go", "Map"); ({ syms: syms, fn: fn });`,
	})
	if resGo.Error != "" {
		t.Fatalf("Go generic readFn failed: %s", resGo.Error)
	}
	resGoMap := resGo.Result.(map[string]any)
	symsGo := marshalCompactNoEscape(resGoMap["syms"])
	fnGo := marshalCompactNoEscape(resGoMap["fn"])
	if strings.Contains(symsGo, "fakeInRawString") || strings.Contains(symsGo, `"name":"func"`) {
		t.Fatalf("should only extract real Go func Map, got syms: %s", symsGo)
	}
	if !strings.Contains(fnGo, `"symbol":"Map"`) || !strings.Contains(fnGo, `return nil`) {
		t.Fatalf("expected full generic Go func Map body, got: %s", fnGo)
	}
}

func TestGlobIgnoreGrepFnAndShCheck(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "src"), 0755)
	_ = os.MkdirAll(filepath.Join(tmp, "vendor"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "src", "a.js"), []byte("function alpha() {\n  const x = TARGET;\n  const y = TARGET;\n  return x + y;\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "src", "b.js"), []byte("function beta() {\n  return TARGET;\n}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "vendor", "c.js"), []byte("function ignored() { return TARGET; }\n"), 0644)

	// 1. fs.glob with { maxFiles, ignore } + .truncated
	resGlob := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			var all = fs.glob("**/*.js", { ignore: ["vendor/**"] });
			var capped = fs.glob("**/*.js", { maxFiles: 1, ignore: ["vendor/**"] });
			({ all: all, allTrunc: all.truncated, capLen: capped.length, capTrunc: capped.truncated });
		`,
	})
	if resGlob.Error != "" {
		t.Fatalf("fs.glob opts failed: %s", resGlob.Error)
	}
	jg := marshalCompactNoEscape(resGlob.Result)
	if strings.Contains(jg, "vendor/c.js") || !strings.Contains(jg, `"allTrunc":false`) || !strings.Contains(jg, `"capLen":1`) || !strings.Contains(jg, `"capTrunc":true`) {
		t.Fatalf("unexpected glob opts result: %s", jg)
	}

	// 2. fs.grepFn deduplicates hits inside alpha() and attaches hitLines
	resGrepFn := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			var fns = fs.grepFn(/TARGET/, "**/*.js", { ignore: ["vendor/**"], maxFns: 5 });
			({ count: fns.length, f0: fns[0].symbol, h0: fns[0].hitLines, f1: fns[1].symbol, trunc: fns.truncated });
		`,
	})
	if resGrepFn.Error != "" {
		t.Fatalf("fs.grepFn failed: %s", resGrepFn.Error)
	}
	jgf := marshalCompactNoEscape(resGrepFn.Result)
	if !strings.Contains(jgf, `"count":2`) || !strings.Contains(jgf, `"f0":"alpha"`) || !strings.Contains(jgf, `"h0":[2,3]`) || !strings.Contains(jgf, `"f1":"beta"`) {
		t.Fatalf("unexpected grepFn result: %s", jgf)
	}

	// 3. sh.check distills failure lines + tail
	resCheck := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `sh.check("printf 'ok 1\\nFAIL test_auth\\nok 2\\nAssertionError: bad token\\ndone\\n'; exit 1");`,
	})
	if resCheck.Error != "" {
		t.Fatalf("sh.check failed: %s", resCheck.Error)
	}
	jc := marshalCompactNoEscape(resCheck.Result)
	if !strings.Contains(jc, `"ok":false`) || !strings.Contains(jc, `"code":1`) || !strings.Contains(jc, `"matchCount":2`) || !strings.Contains(jc, `FAIL test_auth`) {
		t.Fatalf("unexpected sh.check result: %s", jc)
	}
}

func TestEditSafetyGuardsNetDiffsAndRevert(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "vars.js")
	orig := "const id = 1;\nconst width = 10;\nconst valid = id + width;\nreturn id;\n"
	_ = os.WriteFile(fp, []byte(orig), 0644)

	// 1. wholeWord: true renames identifier `id` without corrupting `width` or `valid`!
	resWW := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("vars.js", "id", "userId", { wholeWord: true, expectCount: 3 });`,
	})
	if resWW.Error != "" {
		t.Fatalf("wholeWord edit failed: %s", resWW.Error)
	}
	afterWW, _ := os.ReadFile(fp)
	wantWW := "const userId = 1;\nconst width = 10;\nconst valid = userId + width;\nreturn userId;\n"
	if string(afterWW) != wantWW {
		t.Fatalf("wholeWord corrupted file:\ngot:  %q\nwant: %q", string(afterWW), wantWW)
	}

	// 2. maxReplacements rejects unexpectedly broad replaceAll and leaves file untouched
	resMax := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.edit("vars.js", "userId", "uid", { replaceAll: true, maxReplacements: 2 });`,
	})
	if !strings.Contains(resMax.Error, "exceeds maxReplacements=2") {
		t.Fatalf("expected maxReplacements error, got: %+v", resMax)
	}

	// 3. editLines expectEnd rejects shifted closing line
	resEnd := runCodeExec(ExecRequest{
		Cwd:  tmp,
		Code: `fs.editLines("vars.js", 1, 4, "// replaced", { expectStart: "const userId", expectEnd: "WRONG_END" });`,
	})
	if !strings.Contains(resEnd.Error, "does not contain expectEnd") {
		t.Fatalf("expected expectEnd mismatch error, got: %+v", resEnd)
	}

	// 4. Net-per-file diffs: two edits to same file produce ONE diff, not two
	resNet := runCodeExec(ExecRequest{
		Cwd:      tmp,
		DiffMode: "stat",
		Code: `
			fs.edit("vars.js", "const userId = 1;", "const userId = 2;");
			fs.edit("vars.js", "const width = 10;", "const width = 20;");
		`,
	})
	if resNet.Error != "" {
		t.Fatalf("net diff edit failed: %s", resNet.Error)
	}
	if len(resNet.Diffs) != 1 || resNet.Diffs[0] != "vars.js (+2/-2 lines)" {
		t.Fatalf("expected single consolidated net diff, got: %#v", resNet.Diffs)
	}

	// 5. fs.revert() restores disk even AFTER sh.run() auto-committed edits!
	beforeRevert, _ := os.ReadFile(fp)
	resRev := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			fs.edit("vars.js", "const userId = 2;", "const userId = 999;");
			fs.write("temp_new.js", "hello");
			var chk = sh.check("grep 999 vars.js && exit 1");
			if (!chk.ok) fs.revert();
		`,
	})
	if resRev.Error != "" {
		t.Fatalf("fs.revert failed: %s", resRev.Error)
	}
	afterRevert, _ := os.ReadFile(fp)
	if string(afterRevert) != string(beforeRevert) {
		t.Fatalf("fs.revert did not restore vars.js:\ngot:  %q\nwant: %q", string(afterRevert), string(beforeRevert))
	}
	if _, err := os.Stat(filepath.Join(tmp, "temp_new.js")); !os.IsNotExist(err) {
		t.Fatalf("fs.revert did not remove newly created temp_new.js")
	}
}

func TestGoASTFindSymbolCallersFlexibleOptsAndCheckContext(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "pkg"), 0755)
	_ = os.MkdirAll(filepath.Join(tmp, "web"), 0755)

	svcGo := `package pkg

type Status int
type Alias = Status

const DefaultTimeout = 30

type Client struct{}
type Server struct{}

func (c *Client) Start() string {
	return "client"
}

func (s *Server) Start() string {
	return "server"
}
`
	callerGo := `package pkg

// Start is mentioned in a comment and should be skipped
func RunAll(c *Client, s *Server) string {
	a := c.Start()
	b := s.Start()
	return a + b
}
`
	cfgJS := `export const SHRINE_POWERUPS = Object.freeze({
  frost_bastion: { id: "frost_bastion" },
});
export const helper = () => 42;
`
	_ = os.WriteFile(filepath.Join(tmp, "pkg", "svc.go"), []byte(svcGo), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "pkg", "caller.go"), []byte(callerGo), 0644)
	_ = os.WriteFile(filepath.Join(tmp, "web", "config.js"), []byte(cfgJS), 0644)

	// 1. Go AST extracts receivers, type aliases, consts, and supports receiver-qualified readFn
	resAST := runCodeExec(ExecRequest{
		Cwd:      tmp,
		ReadOnly: true,
		Code: `
			var syms = fs.symbols("pkg/svc.go");
			var srvStart = fs.readFn("pkg/svc.go", "(*Server).Start");
			var cliStart = fs.readFn("pkg/svc.go", "Client.Start");
			var jsSyms = fs.symbols("web/config.js");
			({ syms: syms, srvStart: srvStart, cliStart: cliStart, jsSyms: jsSyms });
		`,
	})
	if resAST.Error != "" {
		t.Fatalf("Go AST / JS const symbols failed: %s", resAST.Error)
	}
	jAST := marshalCompactNoEscape(resAST.Result)
	if !strings.Contains(jAST, `"receiver":"*Client"`) || !strings.Contains(jAST, `"receiver":"*Server"`) || !strings.Contains(jAST, `"kind":"const","line":6,"name":"DefaultTimeout"`) || !strings.Contains(jAST, `"kind":"type","line":4,"name":"Alias"`) {
		t.Fatalf("expected Go AST receivers/const/type alias, got: %s", jAST)
	}
	if !strings.Contains(jAST, `return \"server\"`) || !strings.Contains(jAST, `return \"client\"`) {
		t.Fatalf("expected receiver-qualified readFn to distinguish Client.Start vs (*Server).Start, got: %s", jAST)
	}
	if !strings.Contains(jAST, `"kind":"const","line":1,"name":"SHRINE_POWERUPS"`) || !strings.Contains(jAST, `"kind":"function","line":4,"name":"helper"`) {
		t.Fatalf("expected JS exported const and arrow fn symbols, got: %s", jAST)
	}

	// 2. fs.grep and fs.grepFn accept { glob } options object as 2nd argument
	resOpts := runCodeExec(ExecRequest{
		Cwd:      tmp,
		ReadOnly: true,
		Code: `
			var g = fs.grep("Start", { glob: "**/*.go", maxHits: 10 });
			var gf = fs.grepFn("Start", { glob: "**/*.go", maxFns: 5 });
			({ gLen: g.length, gfLen: gf.length });
		`,
	})
	if resOpts.Error != "" {
		t.Fatalf("flexible { glob } options failed: %s", resOpts.Error)
	}
	jOpts := marshalCompactNoEscape(resOpts.Result)
	if !strings.Contains(jOpts, `"gLen":5`) || !strings.Contains(jOpts, `"gfLen":4`) {
		t.Fatalf("unexpected flexible opts grep/grepFn result: %s", jOpts)
	}

	// 3. fs.findSymbol and fs.callers across files
	resNav := runCodeExec(ExecRequest{
		Cwd:      tmp,
		ReadOnly: true,
		Code: `
			var defs = fs.findSymbol("Start", { glob: "**/*.go" });
			var srvDef = fs.findSymbol("(*Server).Start", "**/*.go", { includeText: true });
			var callers = fs.callers("Start", "**/*.go");
			({ defs: defs, srvDef: srvDef, callers: callers });
		`,
	})
	if resNav.Error != "" {
		t.Fatalf("findSymbol/callers failed: %s", resNav.Error)
	}
	jNav := marshalCompactNoEscape(resNav.Result)
	if !strings.Contains(jNav, `"receiver":"*Server"`) || !strings.Contains(jNav, `return \"server\"`) {
		t.Fatalf("expected findSymbol to return receiver-qualified definition with text, got: %s", jNav)
	}
	if !strings.Contains(jNav, `"caller":"RunAll"`) || !strings.Contains(jNav, `"hitLines":[5,6]`) || strings.Contains(jNav, `"hitLines":[3`) {
		t.Fatalf("expected callers to group RunAll lines [5,6] and skip comment/definition lines, got: %s", jNav)
	}

	// 4. sh.check captures failure context lines after --- FAIL while keeping matchCount exact
	resChk := runCodeExec(ExecRequest{
		Cwd: tmp,
		Code: `
			var c1 = sh.check("printf '=== RUN   TestDemo\\n--- FAIL: TestDemo (0.01s)\\n    demo_test.go:42: got 1, want 2\\n    extra detail\\nPASS\\n'; exit 1");
			var c0 = sh.check("printf '--- FAIL: TestDemo\\n    demo_test.go:42: got 1, want 2\\n'; exit 1", { contextLines: 0 });
			({ c1: c1, c0: c0 });
		`,
	})
	if resChk.Error != "" {
		t.Fatalf("sh.check contextLines failed: %s", resChk.Error)
	}
	jChk := marshalCompactNoEscape(resChk.Result)
	if !strings.Contains(jChk, `"matchCount":1`) || !strings.Contains(jChk, `demo_test.go:42: got 1, want 2`) {
		t.Fatalf("expected sh.check to capture following assertion detail with matchCount=1, got: %s", jChk)
	}
	c0Matches := marshalCompactNoEscape(resChk.Result.(map[string]any)["c0"].(map[string]any)["matches"])
	if strings.Contains(c0Matches, "demo_test.go:42") {
		t.Fatalf("expected contextLines:0 to omit following lines, got: %s", c0Matches)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
