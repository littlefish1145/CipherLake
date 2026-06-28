package flow

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type SecretProvider interface {
	Get(key string) (string, bool)
	Set(key, value string) error
	Delete(key string) error
	List() ([]string, error)
	Audit() []SecretAudit
	Close() error
}

type SecretAudit struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Key    string    `json:"key"`
	Actor  string    `json:"actor,omitempty"`
}

type envSecretProvider struct {
	mu    sync.RWMutex
	store map[string]string
	audit []SecretAudit
}

func NewEnvSecretProvider() SecretProvider {
	return &envSecretProvider{
		store: make(map[string]string),
	}
}

func (p *envSecretProvider) Get(key string) (string, bool) {
	prefixed := "NEXUS_SECRET_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	val, ok := os.LookupEnv(prefixed)
	if ok {
		p.mu.Lock()
		p.audit = append(p.audit, SecretAudit{
			Time: time.Now(), Action: "get", Key: key,
		})
		p.mu.Unlock()
		return val, true
	}

	p.mu.RLock()
	val, ok = p.store[key]
	p.mu.RUnlock()
	return val, ok
}

func (p *envSecretProvider) Set(key, value string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store[key] = value
	p.audit = append(p.audit, SecretAudit{
		Time: time.Now(), Action: "set", Key: key,
	})
	return nil
}

func (p *envSecretProvider) Delete(key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.store, key)
	p.audit = append(p.audit, SecretAudit{
		Time: time.Now(), Action: "delete", Key: key,
	})
	return nil
}

func (p *envSecretProvider) List() ([]string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var keys []string
	for k := range p.store {
		keys = append(keys, k)
	}
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "NEXUS_SECRET_") {
			parts := strings.SplitN(env, "=", 2)
			key := strings.TrimPrefix(parts[0], "NEXUS_SECRET_")
			key = strings.ToLower(strings.ReplaceAll(key, "_", "."))
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (p *envSecretProvider) Audit() []SecretAudit {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]SecretAudit, len(p.audit))
	copy(result, p.audit)
	return result
}

func (p *envSecretProvider) Close() error { return nil }

type WorkflowSecrets struct {
	provider SecretProvider
	telemetry *Telemetry
}

func NewWorkflowSecrets(provider SecretProvider, telemetry *Telemetry) *WorkflowSecrets {
	return &WorkflowSecrets{
		provider:  provider,
		telemetry: telemetry,
	}
}

func (ws *WorkflowSecrets) Resolve(input string) (string, bool) {
	if !strings.HasPrefix(input, "secret:") {
		return input, true
	}

	key := strings.TrimPrefix(input, "secret:")
	val, ok := ws.provider.Get(key)
	if !ok {
		return "", false
	}

	if ws.telemetry != nil {
		ws.telemetry.EventBus.Publish(Event{
			ID:   newID(),
			Type: EventSecretUsed,
			Time: time.Now(),
			Metadata: map[string]string{"key": key},
		})
	}

	return val, true
}

func (ws *WorkflowSecrets) ResolveMap(input map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(input))
	for k, v := range input {
		resolved, ok := ws.Resolve(v)
		if !ok {
			return nil, fmt.Errorf("secret %q not found", strings.TrimPrefix(v, "secret:"))
		}
		result[k] = resolved
	}
	return result, nil
}
