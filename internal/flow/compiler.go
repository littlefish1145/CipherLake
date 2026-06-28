package flow

import (
	"fmt"
	"mime"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type CompiledWorkflow struct {
	Workflow Workflow
	Steps    []CompiledStep
	Name     string
}

func newCompiledWorkflow(w Workflow, steps []CompiledStep) *CompiledWorkflow {
	return &CompiledWorkflow{Workflow: w, Steps: steps, Name: w.Name}
}

type CompiledStep struct {
	Step   Step
	Plugin WorkflowPlugin
	Deps   []int
}

func LoadConfigFile(path string) (*WorkflowConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	return LoadConfigData(data)
}

func LoadConfigData(data []byte) (*WorkflowConfig, error) {
	var cfg WorkflowConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return &cfg, nil
}

type Compiler struct {
	plugins map[string]WorkflowPlugin
}

func NewCompiler(plugins map[string]WorkflowPlugin) *Compiler {
	return &Compiler{plugins: plugins}
}

func (c *Compiler) Compile(cfg *WorkflowConfig) ([]*CompiledWorkflow, error) {
	var result []*CompiledWorkflow
	for _, w := range cfg.Workflows {
		if !w.Enabled {
			continue
		}
		cw, err := c.compileOne(w)
		if err != nil {
			return nil, fmt.Errorf("workflow %q: %w", w.Name, err)
		}
		result = append(result, cw)
	}
	return result, nil
}

func (c *Compiler) compileOne(w Workflow) (*CompiledWorkflow, error) {
	if len(w.Steps) == 0 {
		return nil, ErrInvalidWorkflow
	}

	var compiledSteps []CompiledStep
	for i, s := range w.Steps {
		plugin, ok := c.plugins[s.Plugin]
		if !ok {
			return nil, fmt.Errorf("step %d: plugin %q not found: %w", i, s.Plugin, ErrPluginNotFound)
		}

		cs := CompiledStep{
			Step:   s,
			Plugin: plugin,
			Deps:   c.resolveDeps(w.Steps, i),
		}
		compiledSteps = append(compiledSteps, cs)
	}
	return newCompiledWorkflow(w, compiledSteps), nil
}

func (c *Compiler) resolveDeps(steps []Step, idx int) []int {
	var deps []int
	if idx > 0 {
		deps = append(deps, idx-1)
	}
	return deps
}

func MatchFilter(filter, contentType string, metadata map[string]string) bool {
	if filter == "" {
		return true
	}
	filter = strings.ToLower(filter)
	parts := strings.Split(filter, "and")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "content-type") {
			val := extractFilterValue(part)
			if val != "" && !matchContentType(contentType, val) {
				return false
			}
		}
		if strings.HasPrefix(part, "metadata.") {
			ps := strings.Split(part, "=")
			if len(ps) == 2 {
				key := strings.TrimPrefix(strings.TrimSpace(ps[0]), "metadata.")
				val := strings.TrimSpace(ps[1])
				if metadata[key] != val {
					return false
				}
			}
		}
	}
	return true
}

func extractFilterValue(part string) string {
	for _, q := range []string{"'", "\""} {
		if idx := strings.Index(part, q); idx != -1 {
			rest := part[idx+1:]
			if end := strings.LastIndex(rest, q); end != -1 {
				return rest[:end]
			}
			return rest
		}
	}
	idx := strings.Index(part, "matches")
	if idx != -1 {
		return strings.TrimSpace(part[idx+7:])
	}
	return ""
}

func expandOutputPath(pattern, originalKey string, output *ObjectOutput) string {
	result := pattern
	result = strings.ReplaceAll(result, "{key}", originalKey)
	result = strings.ReplaceAll(result, "{uuid}", newID()[:8])
	if output != nil && output.ContentType != "" {
		if ext := extensionFromMime(output.ContentType); ext != "" {
			result = result + "." + ext
		}
	}
	return result
}

func extensionFromMime(mimeType string) string {
	exts, _ := mime.ExtensionsByType(mimeType)
	if len(exts) > 0 {
		return strings.TrimPrefix(exts[0], ".")
	}
	return ""
}

func matchContentType(contentType, pattern string) bool {
	if pattern == "*/*" || pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(strings.ToLower(contentType), prefix+"/")
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(strings.ToLower(contentType), prefix)
	}
	return strings.Contains(strings.ToLower(contentType), strings.ToLower(pattern))
}
