package app

import (
	"bytes"
	"encoding/json"
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
