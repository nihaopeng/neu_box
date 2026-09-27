package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/neusbox/neu_box/client/neubox/internal/api"
)

func (a *app) workerFailure(status int, raw []byte) int {
	message, code := api.ErrorDetails(status, raw)
	if a.jsonOutput {
		output := map[string]any{
			"error":       message,
			"http_status": status,
		}
		if code != "" {
			output["code"] = code
		}
		_ = printJSONValue(a.errOut, output)
		return 1
	}
	fields := []outputField{{"error", message}, {"http_status", strconv.Itoa(status)}}
	if code != "" {
		fields = append(fields, outputField{"code", code})
	}
	printFields(a.errOut, fields...)
	return 1
}

type outputField struct {
	key   string
	value string
}

// printFields is the one human-readable output layout: two flush-left columns
// with labels aligned. --json remains the contract for machine consumers.
func printFields(writer io.Writer, fields ...outputField) {
	width := 0
	for _, field := range fields {
		if len(field.key) > width {
			width = len(field.key)
		}
	}
	for _, field := range fields {
		fmt.Fprintf(writer, "%-*s %s\n", width+1, field.key+":", field.value)
	}
}

func formatDevices(devices []string) string {
	if len(devices) == 0 {
		return "none"
	}
	return strings.Join(devices, ", ")
}

func formatCPU(cpu int) string {
	if cpu == 0 {
		return "unlimited"
	}
	return strconv.Itoa(cpu)
}

func formatMemory(memory string) string {
	if memory == "" || memory == "0" {
		return "unlimited"
	}
	return memory
}

func printJSON(writer io.Writer, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		fmt.Fprintln(writer, "{}")
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		_, _ = writer.Write(raw)
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			fmt.Fprintln(writer)
		}
		return err
	}
	return printJSONValue(writer, value)
}

func printJSONValue(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
