package actions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// varResolver rewrites {{$...}} tokens in free-text inputs against the flow
// scope, so the Cypher and every parameter can reference upstream data the same
// way. It holds a per-call cache so each distinct path is fetched from the
// runtime only once, however many fields reference it.
type varResolver struct {
	job   *sdkv1.Job
	cache map[string]string
}

func newVarResolver(job *sdkv1.Job) *varResolver {
	return &varResolver{job: job, cache: make(map[string]string)}
}

// resolve substitutes every {{$...}} token in text. Tokens the scope can't
// supply are left verbatim so nothing is silently dropped.
func (vr *varResolver) resolve(text string) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	return varRe.ReplaceAllStringFunc(text, func(tok string) string {
		path := strings.TrimSpace(varRe.FindStringSubmatch(tok)[1])
		v, ok := vr.cache[path]
		if !ok {
			v = vr.fetch(path)
			vr.cache[path] = v
		}
		return v
	})
}

// fetch reads a JSON path from the flow context. The reply is JSON: a JSON
// string is unwrapped to its value, anything else is returned raw so it can be
// inlined into the field.
//
// That raw form is what makes a token usable inside a JSON field: a number in
// the scope lands in `{"id": {{$.user.id}}}` as a number, while a string lands
// in `{"name": "{{$.user.name}}"}` already quoted by the author.
func (vr *varResolver) fetch(jsonPath string) string {
	raw, ok := vr.job.CmdGetScope(jsonPath).([]byte)
	if !ok || len(raw) == 0 {
		return fmt.Sprintf("{{%s}}", jsonPath) // leave the token in place
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// resolveInputVars walks the decoded action input and rewrites {{$...}} tokens
// in every string and []string field. Plain fields (no token) never hit the
// runtime, so this is safe to run over every action's input uniformly.
//
// Values nested inside a map field are not reached — a map's contents are
// resolved by the action that owns it, with a resolver of its own.
func resolveInputVars(job *sdkv1.Job, in any) {
	v := reflect.ValueOf(in)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return
	}

	vr := newVarResolver(job)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch {
		case f.Kind() == reflect.String:
			f.SetString(vr.resolve(f.String()))
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			for j := 0; j < f.Len(); j++ {
				el := f.Index(j)
				el.SetString(vr.resolve(el.String()))
			}
		}
	}
}
