package app

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInitializeAndListTools(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n")
	var output bytes.Buffer
	if err := serveMCP(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d responses, want 2: %s", len(lines), output.String())
	}
	var initialized struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Result.ProtocolVersion != mcpProtocolVersion {
		t.Fatalf("protocol version = %q", initialized.Result.ProtocolVersion)
	}
	var listed struct {
		Result struct {
			Tools []mcpTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) < 12 {
		t.Fatalf("got %d tools, want at least 12", len(listed.Result.Tools))
	}
	if listed.Result.Tools[0].Name != "spm_list_services" {
		t.Fatalf("first tool = %q", listed.Result.Tools[0].Name)
	}
}

func TestMCPUnknownToolIsToolError(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"missing","arguments":{}}}`
	var output bytes.Buffer
	if err := serveMCP(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.IsError {
		t.Fatal("unknown tool did not return an MCP tool error")
	}
}

func TestMCPRejectsUnknownArguments(t *testing.T) {
	result := callSPMTool("spm_show_service", json.RawMessage(`{"name":"api","unexpected":true}`))
	if result["isError"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestMCPRunCLICoversAdvancedCommands(t *testing.T) {
	result := callSPMTool("spm_run_cli", json.RawMessage(`{"arguments":["help","schedule"]}`))
	if result["isError"] == true {
		t.Fatalf("result = %#v", result)
	}
	content := result["content"].([]map[string]any)
	if !strings.Contains(content[0]["text"].(string), "spm schedule") {
		t.Fatalf("unexpected help output: %#v", result)
	}
}

func TestMCPRunCLIRejectsInteractiveAndUnboundedCommands(t *testing.T) {
	tests := [][]string{
		{"mcp"},
		{"edit", "api"},
		{"logs", "api", "--follow"},
		{"list", "--watch=1s"},
		{"--system", "list"},
	}
	for _, arguments := range tests {
		raw, err := json.Marshal(map[string]any{"arguments": arguments})
		if err != nil {
			t.Fatal(err)
		}
		result := callSPMTool("spm_run_cli", raw)
		if result["isError"] != true {
			t.Errorf("arguments %v were not rejected: %#v", arguments, result)
		}
	}
}

func TestMCPProtocolErrors(t *testing.T) {
	input := strings.Join([]string{
		`{not-json}`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"missing"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":3}}`,
	}, "\n")
	var output bytes.Buffer
	if err := serveMCP(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d responses, want 4: %s", len(lines), output.String())
	}
	wantCodes := []int{-32700, -32600, -32601, -32602}
	for i, line := range lines {
		var response struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatal(err)
		}
		if response.Error.Code != wantCodes[i] {
			t.Errorf("response %d code = %d, want %d", i, response.Error.Code, wantCodes[i])
		}
	}
}

func TestDecodeMCPArgumentsRejectsTrailingData(t *testing.T) {
	var args mcpNameArgs
	if err := decodeMCPArguments(json.RawMessage(`{"name":"api"} {}`), &args); err == nil || !strings.Contains(err.Error(), "unexpected data") {
		t.Fatalf("decodeMCPArguments() error = %v", err)
	}
	if err := decodeMCPArguments(nil, &args); err != nil {
		t.Fatalf("empty arguments should decode as an empty object: %v", err)
	}
}

func TestMCPFocusedToolValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"spm_create_service", `{}`},
		{"spm_get_logs", `{"lines":1001}`},
		{"spm_show_service", `{"name":"api"} {}`},
		{"missing", `{}`},
	}
	for _, test := range tests {
		result := callSPMTool(test.name, json.RawMessage(test.raw))
		if result["isError"] != true {
			t.Errorf("%s(%s) was not rejected: %#v", test.name, test.raw, result)
		}
	}
}

func TestMCPToolDispatchCoversFocusedTools(t *testing.T) {
	root := localTempDir(t)
	t.Setenv("SPM_WINDOWS_DATA_DIR", root)
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	tests := []struct {
		name string
		raw  string
	}{
		{"spm_list_services", `{}`},
		{"spm_show_service", `{"name":"missing"}`},
		{"spm_get_logs", `{"names":["missing"],"lines":25,"since":"1h","grep":"error","health":true}`},
		{"spm_doctor", `{}`},
		{"spm_create_service", `{"name":"api","command":["missing-command"],"cwd":".","envFile":".env","restart":"bad","force":true}`},
		{"spm_start_service", `{"name":"missing"}`},
		{"spm_restart_service", `{"name":"missing"}`},
		{"spm_stop_service", `{"name":"missing"}`},
		{"spm_enable_service", `{"name":"missing"}`},
		{"spm_disable_service", `{"name":"missing"}`},
		{"spm_delete_service", `{"name":"missing"}`},
		{"spm_run_cli", `{"arguments":["help","start"]}`},
	}
	for _, test := range tests {
		result := callSPMTool(test.name, json.RawMessage(test.raw))
		if result["content"] == nil {
			t.Errorf("%s returned no content: %#v", test.name, result)
		}
	}
	if result := callSPMTool("spm_get_logs", json.RawMessage(`{}`)); result["content"] == nil {
		t.Fatalf("default log arguments returned %#v", result)
	}
}

func FuzzDecodeMCPArguments(f *testing.F) {
	for _, seed := range []string{`{}`, `null`, `{"name":"api","system":true}`, `{"unknown":1}`, `{`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var args mcpNameArgs
		// JSON received over stdio is untrusted and must never panic the server.
		_ = decodeMCPArguments(json.RawMessage(input), &args)
	})
}
