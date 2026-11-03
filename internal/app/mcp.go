package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const mcpProtocolVersion = "2025-06-18"

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func serveMCP(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var request mcpRequest
		if err := json.Unmarshal(line, &request); err != nil {
			if err := writeMCPError(encoder, nil, -32700, "Parse error", err.Error()); err != nil {
				return err
			}
			continue
		}
		if request.JSONRPC != "2.0" || request.Method == "" {
			if err := writeMCPError(encoder, request.ID, -32600, "Invalid Request", nil); err != nil {
				return err
			}
			continue
		}
		if len(request.ID) == 0 {
			// MCP notifications never receive a response.
			continue
		}
		if err := handleMCPRequest(encoder, request); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handleMCPRequest(encoder *json.Encoder, request mcpRequest) error {
	switch request.Method {
	case "initialize":
		return writeMCPResult(encoder, request.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "spm", "version": version},
			"instructions": "Use spm tools to inspect and manage persistent services. Prefer focused read-only tools first, then focused write tools. Use spm_run_cli only for advanced operations without a dedicated tool. Confirm the target and scope before state-changing or destructive calls.",
		})
	case "ping":
		return writeMCPResult(encoder, request.ID, map[string]any{})
	case "tools/list":
		return writeMCPResult(encoder, request.ID, map[string]any{"tools": spmMCPTools()})
	case "tools/call":
		var params mcpCallParams
		if err := decodeMCPArguments(request.Params, &params); err != nil {
			return writeMCPError(encoder, request.ID, -32602, "Invalid params", err.Error())
		}
		result := callSPMTool(params.Name, params.Arguments)
		return writeMCPResult(encoder, request.ID, result)
	default:
		return writeMCPError(encoder, request.ID, -32601, "Method not found", request.Method)
	}
}

func writeMCPResult(encoder *json.Encoder, id json.RawMessage, result any) error {
	return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeMCPError(encoder *json.Encoder, id json.RawMessage, code int, message string, data any) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	errorObject := map[string]any{"code": code, "message": message}
	if data != nil {
		errorObject["data"] = data
	}
	return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": errorObject})
}

func spmMCPTools() []mcpTool {
	readOnly := map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}
	write := map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false}
	destructive := map[string]any{"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false}
	fullCLI := map[string]any{"readOnlyHint": false, "destructiveHint": true, "openWorldHint": true}
	return []mcpTool{
		mcpToolDefinition("spm_list_services", "List services", "List services managed by spm and return their runtime state as JSON.", scopeSchema(), readOnly),
		mcpToolDefinition("spm_show_service", "Show service", "Show configuration and runtime details for one managed service.", namedScopeSchema(), readOnly),
		mcpToolDefinition("spm_get_logs", "Get service logs", "Read a bounded number of recent service or health-check log entries. This tool never follows logs continuously.", objectSchema(map[string]any{
			"names":  stringArrayProperty("Service names. Omit to read logs for all managed services."),
			"lines":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 100},
			"since":  map[string]any{"type": "string", "description": "Optional journal time expression such as 1h or 2026-01-01."},
			"grep":   map[string]any{"type": "string", "description": "Optional journal grep expression."},
			"health": map[string]any{"type": "boolean", "default": false},
			"system": systemProperty(),
		}, nil), readOnly),
		mcpToolDefinition("spm_doctor", "Diagnose spm", "Run spm environment and service-manager diagnostics.", scopeSchema(), readOnly),
		mcpToolDefinition("spm_create_service", "Create service", "Create and start a persistent service from an explicit command. Use env_file for secrets instead of passing secret values.", objectSchema(map[string]any{
			"name":     nameProperty(),
			"command":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Executable and arguments as separate array elements."},
			"cwd":      map[string]any{"type": "string"},
			"env_file": map[string]any{"type": "string", "description": "Path to an environment file. Do not place secret values directly in MCP arguments."},
			"restart":  map[string]any{"type": "string", "enum": []string{"no", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog", "always"}},
			"force":    map[string]any{"type": "boolean", "default": false, "description": "Replace an existing service definition."},
			"system":   systemProperty(),
		}, []string{"name", "command"}), write),
		mcpToolDefinition("spm_start_service", "Start service", "Start an existing managed service.", namedScopeSchema(), write),
		mcpToolDefinition("spm_restart_service", "Restart service", "Restart an existing managed service.", namedScopeSchema(), write),
		mcpToolDefinition("spm_stop_service", "Stop service", "Stop a service while preserving its boot enablement.", namedScopeSchema(), write),
		mcpToolDefinition("spm_enable_service", "Enable service", "Enable and start a managed service.", namedScopeSchema(), write),
		mcpToolDefinition("spm_disable_service", "Disable service", "Stop a managed service and disable boot startup.", namedScopeSchema(), write),
		mcpToolDefinition("spm_delete_service", "Delete service", "Stop and permanently remove a managed service and its companion units.", namedScopeSchema(), destructive),
		mcpToolDefinition("spm_run_cli", "Run advanced spm command", "Run any supported non-interactive spm CLI operation, including schedule, signal, scale, set, unset, clone, history, rollback, apply, validate, diff, native inspection, security analysis, export, health, and import. Prefer focused tools when one exists. Interactive editors and unbounded watch/follow modes are rejected.", objectSchema(map[string]any{
			"arguments": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Arguments exactly as they appear after the spm executable, excluding --system. Example: [\"schedule\", \"--name\", \"backup\", \"--cron\", \"0 2 * * *\", \"--\", \"backup.exe\"]."},
			"system":    systemProperty(),
		}, []string{"arguments"}), fullCLI),
	}
}

func mcpToolDefinition(name, title, description string, schema map[string]any, annotations map[string]any) mcpTool {
	return mcpTool{Name: name, Title: title, Description: description, InputSchema: schema, Annotations: annotations}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func scopeSchema() map[string]any {
	return objectSchema(map[string]any{"system": systemProperty()}, nil)
}

func namedScopeSchema() map[string]any {
	return objectSchema(map[string]any{"name": nameProperty(), "system": systemProperty()}, []string{"name"})
}

func nameProperty() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "description": "Managed service name."}
}

func systemProperty() map[string]any {
	return map[string]any{"type": "boolean", "default": false, "description": "Manage machine-wide services instead of current-user services."}
}

func stringArrayProperty(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
}

type mcpScopeArgs struct {
	System bool `json:"system"`
}

type mcpNameArgs struct {
	Name   string `json:"name"`
	System bool   `json:"system"`
}

type mcpLogsArgs struct {
	Names  []string `json:"names"`
	Lines  int      `json:"lines"`
	Since  string   `json:"since"`
	Grep   string   `json:"grep"`
	Health bool     `json:"health"`
	System bool     `json:"system"`
}

type mcpCreateArgs struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	EnvFile string   `json:"env_file"`
	Restart string   `json:"restart"`
	Force   bool     `json:"force"`
	System  bool     `json:"system"`
}

type mcpCLIArgs struct {
	Arguments []string `json:"arguments"`
	System    bool     `json:"system"`
}

func callSPMTool(name string, raw json.RawMessage) map[string]any {
	var output string
	var err error
	switch name {
	case "spm_list_services":
		var args mcpScopeArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			output, err = runSPMForMCP(args.System, "list", "--json")
			if err == nil {
				var services []any
				if json.Unmarshal([]byte(output), &services) == nil {
					return map[string]any{
						"structuredContent": map[string]any{"services": services},
						"content":           []map[string]any{{"type": "text", "text": output}},
					}
				}
			}
		}
	case "spm_show_service":
		var args mcpNameArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			output, err = runSPMForMCP(args.System, "show", args.Name)
		}
	case "spm_get_logs":
		var args mcpLogsArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			if args.Lines == 0 {
				args.Lines = 100
			}
			if args.Lines < 1 || args.Lines > 1000 {
				err = errors.New("lines must be between 1 and 1000")
				break
			}
			command := []string{"logs", "--lines", fmt.Sprint(args.Lines)}
			if args.Since != "" {
				command = append(command, "--since", args.Since)
			}
			if args.Grep != "" {
				command = append(command, "--grep", args.Grep)
			}
			if args.Health {
				command = append(command, "--health")
			}
			command = append(command, args.Names...)
			output, err = runSPMForMCP(args.System, command...)
		}
	case "spm_doctor":
		var args mcpScopeArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			output, err = runSPMForMCP(args.System, "doctor")
		}
	case "spm_create_service":
		var args mcpCreateArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			if args.Name == "" || len(args.Command) == 0 {
				err = errors.New("name and command are required")
				break
			}
			command := []string{"start", "--name", args.Name}
			if args.Cwd != "" {
				command = append(command, "--cwd", args.Cwd)
			}
			if args.EnvFile != "" {
				command = append(command, "--env-file", args.EnvFile)
			}
			if args.Restart != "" {
				command = append(command, "--restart", args.Restart)
			}
			if args.Force {
				command = append(command, "--force")
			}
			command = append(command, "--")
			command = append(command, args.Command...)
			output, err = runSPMForMCP(args.System, command...)
		}
	case "spm_start_service", "spm_restart_service", "spm_stop_service", "spm_enable_service", "spm_disable_service", "spm_delete_service":
		var args mcpNameArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			command := strings.TrimSuffix(strings.TrimPrefix(name, "spm_"), "_service")
			output, err = runSPMForMCP(args.System, command, args.Name)
		}
	case "spm_run_cli":
		var args mcpCLIArgs
		if err = decodeMCPArguments(raw, &args); err == nil {
			if err = validateMCPCLIArguments(args.Arguments); err == nil {
				output, err = runSPMForMCP(args.System, args.Arguments...)
			}
		}
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	if strings.TrimSpace(output) == "" {
		output = "spm command completed successfully."
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": output}}}
}

var mcpCLICommands = map[string]bool{
	"start": true, "schedule": true, "run": true,
	"list": true, "ls": true, "status-all": true,
	"signal": true, "kill": true, "scale": true,
	"doctor": true, "status": true, "show": true, "describe": true,
	"restart": true, "stop": true, "enable": true, "disable": true, "reload": true,
	"set": true, "unset": true, "clone": true, "history": true, "rollback": true,
	"delete": true, "remove": true, "rm": true, "logs": true, "cat": true,
	"startup": true, "apply": true, "validate": true, "diff": true,
	"native": true, "security": true, "export": true, "health": true, "import": true,
	"help": true, "version": true, "--version": true, "-v": true,
}

func validateMCPCLIArguments(args []string) error {
	if len(args) == 0 {
		return errors.New("arguments must contain an spm command")
	}
	if args[0] == "--system" {
		return errors.New("use the system boolean instead of passing --system in arguments")
	}
	if !mcpCLICommands[args[0]] {
		return fmt.Errorf("command %q is not available through MCP", args[0])
	}
	for _, arg := range args[1:] {
		key, _, _ := strings.Cut(arg, "=")
		if args[0] == "logs" && (key == "-f" || key == "--follow") {
			return errors.New("unbounded log following is not available through MCP; use spm_get_logs")
		}
		if (args[0] == "list" || args[0] == "ls" || args[0] == "status-all") && (key == "-w" || key == "--watch") {
			return errors.New("unbounded list watching is not available through MCP; use spm_list_services")
		}
	}
	return nil
}

func decodeMCPArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected data after arguments")
		}
		return err
	}
	return nil
}

func runSPMForMCP(system bool, args ...string) (string, error) {
	if system {
		args = append([]string{"--system"}, args...)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run(args, &stdout, &stderr)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return stdout.String(), fmt.Errorf("%s: %w", message, err)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}
