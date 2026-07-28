package pluginloader

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrMissingDependency is returned by InstallPlugin when a manifest declares
// a depends_on plugin that is not currently installed (spec §3.11).
type ErrMissingDependency struct {
	Plugin   string
	Missing  string
}

func (e ErrMissingDependency) Error() string {
	return fmt.Sprintf("plugin %q depends on %q which is not installed", e.Plugin, e.Missing)
}

// IsErrMissingDependency reports whether err is an ErrMissingDependency.
func IsErrMissingDependency(err error) bool {
	var e ErrMissingDependency
	return errors.As(err, &e)
}

// topologicalOrder returns plugin names sorted so that every dependency is
// listed before the plugin that depends on it. It uses Kahn's algorithm and
// detects dependency cycles. Unlisted dependencies (plugins named in depends_on
// but not present in records) are ignored for ordering purposes — InstallPlugin
// rejects them earlier.
func topologicalOrder(records []PluginRecord) ([]string, error) {
	// Build adjacency list and in-degree map.
	graph := make(map[string][]string)
	inDegree := make(map[string]int)
	known := make(map[string]bool)
	for _, r := range records {
		known[r.Name] = true
		if _, ok := inDegree[r.Name]; !ok {
			inDegree[r.Name] = 0
		}
	}

	for _, r := range records {
		var m Manifest
		if err := json.Unmarshal(r.Manifest, &m); err != nil {
			continue
		}
		for _, dep := range m.DependsOn {
			if !known[dep] {
				continue
			}
			graph[dep] = append(graph[dep], r.Name)
			inDegree[r.Name]++
		}
	}

	// Seed queue with zero-in-degree nodes. Stable order by name for determinism.
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	sortStrings(queue)

	var result []string
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		result = append(result, name)

		next := graph[name]
		sortStrings(next)
		for _, child := range next {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}

	if len(result) != len(inDegree) {
		return nil, errors.New("plugin dependency cycle detected")
	}
	return result, nil
}

// dependencyOrder returns the names of installed plugins in dependency order.
func (l *Loader) dependencyOrder() ([]string, error) {
	records, err := l.listPluginRecords()
	if err != nil {
		return nil, err
	}
	return topologicalOrder(records)
}

// checkDependenciesInstalled returns an error if any plugin in deps is not
// currently installed.
func (l *Loader) checkDependenciesInstalled(pluginName string, deps []string) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, dep := range deps {
		if _, ok := l.plugins[dep]; !ok {
			return ErrMissingDependency{Plugin: pluginName, Missing: dep}
		}
	}
	return nil
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
