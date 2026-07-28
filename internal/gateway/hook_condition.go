package gateway

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
)

// HookConditionEnv holds the runtime variables available to a hook CEL
// condition (spec §3.2, P5-4).
type HookConditionEnv struct {
	Operation    string
	Bucket       string
	Key          string
	Method       string
	Body         []byte
	Headers      map[string]string
	UserARN      string
	SourceIP     string
	UserMetadata map[string]string
	Time         time.Time
}

// hookConditionEvaluator compiles and evaluates CEL expressions for hook
// conditions. Programs are cached by expression string.
type hookConditionEvaluator struct {
	env     *cel.Env
	cache   map[string]cel.Program
	cacheMu sync.RWMutex
}

// newHookConditionEvaluator creates an evaluator with the standard hook
// condition variable declarations.
func newHookConditionEvaluator() (*hookConditionEvaluator, error) {
	env, err := cel.NewEnv(
		cel.Declarations(
			decls.NewVar("bucket", decls.String),
			decls.NewVar("key", decls.String),
			decls.NewVar("size", decls.Int),
			decls.NewVar("content_type", decls.String),
			decls.NewVar("user_metadata", decls.NewMapType(decls.String, decls.String)),
			decls.NewVar("user_arn", decls.String),
			decls.NewVar("source_ip", decls.String),
			decls.NewVar("time", decls.Timestamp),
			decls.NewVar("operation", decls.String),
			decls.NewVar("method", decls.String),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL env: %w", err)
	}
	return &hookConditionEvaluator{
		env:   env,
		cache: make(map[string]cel.Program),
	}, nil
}

// evaluate returns true if the CEL expression evaluates to true for the given
// environment. Empty expressions are treated as "always true". Non-boolean
// results or compilation errors return false and an error.
func (e *hookConditionEvaluator) evaluate(expr string, env *HookConditionEnv) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}
	if e == nil || e.env == nil {
		return false, fmt.Errorf("condition evaluator not initialized")
	}

	prg := e.cachedProgram(expr)
	if prg == nil {
		ast, iss := e.env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			return false, fmt.Errorf("failed to compile condition %q: %w", expr, iss.Err())
		}
		var err error
		prg, err = e.env.Program(ast)
		if err != nil {
			return false, fmt.Errorf("failed to program condition %q: %w", expr, err)
		}
		e.storeProgram(expr, prg)
	}

	if env == nil {
		env = &HookConditionEnv{}
	}
	ts := env.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	contentType := ""
	if env.Headers != nil {
		contentType = env.Headers["Content-Type"]
	}
	out, _, err := prg.Eval(map[string]interface{}{
		"operation":     env.Operation,
		"bucket":        env.Bucket,
		"key":           env.Key,
		"method":        env.Method,
		"size":          len(env.Body),
		"content_type":  contentType,
		"user_metadata": env.UserMetadata,
		"user_arn":      env.UserARN,
		"source_ip":     env.SourceIP,
		"time":          ts,
	})
	if err != nil {
		return false, fmt.Errorf("failed to evaluate condition %q: %w", expr, err)
	}
	val, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition %q did not evaluate to bool: %T", expr, out.Value())
	}
	return val, nil
}

func (e *hookConditionEvaluator) cachedProgram(expr string) cel.Program {
	e.cacheMu.RLock()
	defer e.cacheMu.RUnlock()
	return e.cache[expr]
}

func (e *hookConditionEvaluator) storeProgram(expr string, prg cel.Program) {
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	e.cache[expr] = prg
}

// newHookConditionEnvFromRequest builds a condition environment from an HTTP
// request and the target S3 operation/bucket/key. It is used by S3 handlers
// when invoking the hook orchestrator with CEL support.
func newHookConditionEnvFromRequest(r *http.Request, operation, bucket, key string) *HookConditionEnv {
	env := &HookConditionEnv{
		Operation: operation,
		Bucket:    bucket,
		Key:       key,
		Method:    r.Method,
		Headers:   headerToMap(r.Header),
		Time:      time.Now(),
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		env.SourceIP = host
	} else {
		env.SourceIP = r.RemoteAddr
	}
	return env
}
