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
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations"`
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
		withOutputSchema(mcpToolDefinition("spm_list_services", "List services",
			"List every service spm manages in the chosen scope with its live state. Call this first to see what is running, to find an exact service name, or to confirm the effect of a create, start, stop or delete; use spm_show_service for one service's full configuration. "+
				"Returns structuredContent.services with one entry per unit (a multi-instance service has one entry per instance): name, status, active_state, sub_state, health, pid, cpu_percent, memory_bytes, restarts, enabled, uptime_seconds, and schedule for scheduled jobs. Values the platform cannot report are null. An empty array means no services exist in that scope. "+scopeNote,
			scopeSchema(), readOnly), listServicesOutputSchema()),
		mcpToolDefinition("spm_show_service", "Show service",
			"Show one managed service in detail as key-value text: status, enabled, PID, memory, restarts, uptime, last result and exit status, schedule, health, its saved configuration (command, working directory, restart policy and other options), and the paths of its unit and config files. "+
				"Use it before changing or deleting a service, or to find out how a service was started; use spm_list_services to compare many services and spm_get_logs to see why one failed. Fails if the name is not managed by spm in that scope. "+scopeNote,
			namedScopeSchema(), readOnly),
		mcpToolDefinition("spm_get_logs", "Get service logs",
			"Read recent log lines from one or more managed services. Use it to check that a service started cleanly, to find why it crashed or keeps restarting, or to read its output such as URLs, ports and errors; use spm_doctor instead when spm itself or the service manager seems broken. "+
				"Returns at most `lines` entries (default 100) as text; with several names, their entries are combined. Set health=true to read health-check output instead of service output. It never follows logs, so call it again to see newer lines. "+scopeNote,
			objectSchema(map[string]any{
				"names":  stringArrayProperty("Service names to read. Omit to read logs for all managed services in the scope."),
				"lines":  map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 100, "description": "Maximum number of log lines to return."},
				"since":  map[string]any{"type": "string", "description": "Only return entries newer than this: a relative time such as 10m or 1h, or a date such as 2026-01-01."},
				"grep":   map[string]any{"type": "string", "description": "Only return lines matching this pattern, for example error."},
				"health": map[string]any{"type": "boolean", "default": false, "description": "Read health-check logs instead of the service's own output."},
				"system": systemProperty(),
			}, nil), readOnly),
		mcpToolDefinition("spm_doctor", "Diagnose spm",
			"Check that spm can manage services in the chosen scope: whether the service manager is reachable and recent enough, the unit directory, log access, login persistence and the spm executable; the exact checks depend on the platform. "+
				"Use it when a create, start or list call fails unexpectedly, or before the first service on a new machine; use spm_get_logs for problems inside one service. Returns one line per check marked ok, info, warn or fail, with details on what is wrong. "+scopeNote,
			scopeSchema(), readOnly),
		mcpToolDefinition("spm_create_service", "Create service",
			"Create a new persistent service from a command and start it immediately. By default it restarts when it exits and starts again at login, so use it instead of running long-lived processes in the background (&, nohup, start /b) for dev servers, workers and bots that must outlive the session. "+
				"Use spm_start_service or spm_enable_service for a service that already exists. If the name is taken the call fails; set force=true only after the user agrees to replace that service. Put secrets in an env_file, never in command arguments. Afterwards, call spm_get_logs to confirm it started. "+scopeNote,
			objectSchema(map[string]any{
				"name":     map[string]any{"type": "string", "minLength": 1, "description": "New service name, unique in the scope; letters, numbers, dot, underscore and dash."},
				"command":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Executable and arguments as separate array elements, for example [\"npm\", \"run\", \"dev\"]."},
				"cwd":      map[string]any{"type": "string", "description": "Working directory for the command. Defaults to the directory the MCP server runs in, so pass the project directory explicitly."},
				"env_file": map[string]any{"type": "string", "description": "Path to a KEY=VALUE environment file loaded at start. Use this for secrets instead of putting values in MCP arguments."},
				"restart":  map[string]any{"type": "string", "enum": []string{"no", "on-success", "on-failure", "on-abnormal", "on-abort", "on-watchdog", "always"}, "default": "always", "description": "When to restart the process after it exits."},
				"force":    map[string]any{"type": "boolean", "default": false, "description": "Replace an existing service with the same name. Only with the user's agreement."},
				"system":   systemProperty(),
			}, []string{"name", "command"}), write),
		mcpToolDefinition("spm_start_service", "Start service",
			"Start an existing managed service that is stopped. It does not change whether the service starts at login; use spm_enable_service to start it and turn that on, or spm_create_service for a service that does not exist yet. A service that is already running is left as it is; use spm_restart_service to pick up code or configuration changes. "+scopeNote,
			namedScopeSchema(), write),
		mcpToolDefinition("spm_restart_service", "Restart service",
			"Stop and start a managed service so it picks up new code, configuration or environment. The process gets a new PID and is briefly unavailable; a multi-instance service is restarted one instance at a time so the others keep serving. A stopped service is simply started. "+scopeNote,
			namedScopeSchema(), write),
		mcpToolDefinition("spm_stop_service", "Stop service",
			"Stop a running managed service for now. It keeps its definition and still starts again at the next login or boot if it is enabled; use spm_disable_service to keep it stopped, or spm_delete_service to remove it. For a scheduled job, this also stops its timer. "+scopeNote,
			namedScopeSchema(), write),
		mcpToolDefinition("spm_enable_service", "Enable service",
			"Turn on start at login or boot for an existing managed service, and start it now. Use it to undo spm_disable_service; use spm_start_service to run a service now without changing its startup setting. For a scheduled job, this re-enables its timer. "+scopeNote,
			namedScopeSchema(), write),
		mcpToolDefinition("spm_disable_service", "Disable service",
			"Stop a managed service and turn off start at login or boot, so it stays stopped until enabled again. Its definition is kept and spm_enable_service brings it back. Use spm_stop_service for a temporary stop, or spm_delete_service to remove it completely. For a scheduled job, this disables its timer. "+scopeNote,
			namedScopeSchema(), write),
		mcpToolDefinition("spm_delete_service", "Delete service",
			"Stop a managed service and permanently remove its definition, saved configuration and companion units such as schedules and health checks. This cannot be undone; the service must be created again with spm_create_service. Confirm the exact name and scope with the user first, and prefer spm_stop_service or spm_disable_service when the user may need it again. "+scopeNote,
			namedScopeSchema(), destructive),
		mcpToolDefinition("spm_run_cli", "Run advanced spm command",
			"Run any supported non-interactive spm CLI operation, including schedule, signal, scale, set, unset, clone, history, rollback, apply, validate, diff, native inspection, security analysis, export, health, and import. Prefer focused tools when one exists. Interactive editors and unbounded watch/follow modes are rejected. "+
				"Returns the command's text output; run [\"help\", \"<command>\"] first to see a command's options. "+scopeNote,
			objectSchema(map[string]any{
				"arguments": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "Arguments exactly as they appear after the spm executable, excluding --system. Example: [\"schedule\", \"--name\", \"backup\", \"--cron\", \"0 2 * * *\", \"--\", \"backup.exe\"]."},
				"system":    systemProperty(),
			}, []string{"arguments"}), fullCLI),
	}
}

// scopeNote ends every tool description so agents know the default scope and
// what the system flag requires.
const scopeNote = "Acts on the current user's services unless system is true; machine-wide services need root on Linux and macOS or an Administrator shell on Windows, so only use system when the user asks for it."

func mcpToolDefinition(name, title, description string, schema map[string]any, annotations map[string]any) mcpTool {
	return mcpTool{Name: name, Title: title, Description: description, InputSchema: schema, Annotations: annotations}
}

func withOutputSchema(tool mcpTool, schema map[string]any) mcpTool {
	tool.OutputSchema = schema
	return tool
}

// listServicesOutputSchema describes the structuredContent of
// spm_list_services; it mirrors listRow.
func listServicesOutputSchema() map[string]any {
	nullable := func(kind, description string) map[string]any {
		return map[string]any{"type": []string{kind, "null"}, "description": description}
	}
	service := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":           map[string]any{"type": "string", "description": "Service name."},
			"instance":       map[string]any{"type": "integer", "description": "Instance number for multi-instance services."},
			"unit":           map[string]any{"type": "string", "description": "Native unit, job or service identifier."},
			"status":         map[string]any{"type": "string", "description": "Combined state, for example active/running or inactive/dead."},
			"active_state":   map[string]any{"type": "string"},
			"sub_state":      map[string]any{"type": "string"},
			"health":         map[string]any{"type": "string", "description": "Health-check result, or - when none is configured."},
			"pid":            map[string]any{"type": "integer", "description": "Main process ID while running."},
			"cpu_percent":    nullable("number", "Recent CPU usage in percent."),
			"memory_bytes":   nullable("integer", "Current memory usage in bytes."),
			"restarts":       map[string]any{"type": "integer", "description": "Automatic restarts of the current run."},
			"enabled":        map[string]any{"type": "string", "description": "Whether it starts at login or boot, usually enabled or disabled."},
			"uptime_seconds": nullable("integer", "Seconds since the current process started."),
			"schedule":       map[string]any{"type": "string", "description": "Schedule summary for scheduled jobs."},
		},
		"required": []string{"name", "status"},
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"services": map[string]any{"type": "array", "items": service}},
		"required":   []string{"services"},
	}
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
	return map[string]any{"type": "boolean", "default": false, "description": "Manage machine-wide services instead of the current user's. Requires root or Administrator; leave false unless the user asks for system scope."}
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
				if err = json.Unmarshal([]byte(output), &services); err == nil {
					return map[string]any{
						"structuredContent": map[string]any{"services": services},
						"content":           []map[string]any{{"type": "text", "text": output}},
					}
				}
				err = fmt.Errorf("read spm list output: %w", err)
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
