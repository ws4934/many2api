package grok

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// FunctionParametersError carries schema diagnostics without depending on a
// concrete adapter. Build maps these fields to its Responses error envelope.
type FunctionParametersError struct{ Message, Param, Code string }

func (e *FunctionParametersError) Error() string { return e.Message }

const maxRootUnionDepth = 32
const maxRootUnionLeaves = 32

type functionParametersRootContext struct {
	param    string
	toolName string
}

type rootObjectLeafCollector struct {
	doc              map[string]any
	context          functionParametersRootContext
	leaves           []map[string]any
	rootUnionKeyword string
	requiresDisjoint bool
	changed          bool
}

// NormalizeFunctionParametersRoot adapts the shared Grok function-schema grammar.
// Root object unions are lifted; property unions and recursive refs are preserved.
func NormalizeFunctionParametersRoot(value any, param, toolName string) (any, bool, error) {
	schema, ok := value.(map[string]any)
	if !ok {
		return nil, false, invalidFunctionParametersRoot(functionParametersRootContext{param: param, toolName: toolName})
	}
	doc := CloneJSONObject(schema)
	context := functionParametersRootContext{param: param, toolName: toolName}
	collector := rootObjectLeafCollector{doc: doc, context: context}
	if err := collector.walk(doc, nil, nil, 0, 0); err != nil {
		return nil, false, err
	}
	if len(collector.leaves) == 0 {
		return nil, false, invalidFunctionParametersRoot(context)
	}
	if collector.requiresDisjoint && !rootObjectLeavesPairwiseDisjoint(collector.leaves, doc) {
		return nil, false, invalidFunctionParametersRoot(context)
	}
	if !collector.changed {
		return doc, false, nil
	}
	var out map[string]any
	if len(collector.leaves) == 1 {
		out = collector.leaves[0]
	} else {
		if collector.rootUnionKeyword == "" {
			return nil, false, invalidFunctionParametersRoot(context)
		}
		branches := make([]any, len(collector.leaves))
		for i, leaf := range collector.leaves {
			branches[i] = leaf
		}
		out = map[string]any{collector.rootUnionKeyword: branches}
	}
	delete(out, "$defs")
	if defs := referencedDefs(out, doc); len(defs) > 0 {
		out["$defs"] = defs
	}
	return out, !reflect.DeepEqual(out, schema), nil
}

func (c *rootObjectLeafCollector) walk(node any, constraints []map[string]any, seen map[string]struct{}, depth, unionDepth int) error {
	if depth > maxRootUnionDepth || len(c.leaves) > maxRootUnionLeaves {
		return invalidFunctionParametersRoot(c.context)
	}
	schema, ok := node.(map[string]any)
	if !ok {
		return invalidFunctionParametersRoot(c.context)
	}
	if ref, ok := schema["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return invalidFunctionParametersRoot(c.context)
		}
		if _, loop := seen[ref]; loop {
			return invalidFunctionParametersRoot(c.context)
		}
		resolved, ok := ResolveLocalSchemaRef(c.doc, ref)
		if !ok {
			return invalidFunctionParametersRoot(c.context)
		}
		sibling, siblingErr := rootSchemaSiblingConstraint(schema, "$ref")
		if siblingErr != nil {
			return invalidFunctionParametersRoot(c.context)
		}
		if len(sibling) > 0 {
			constraints = append(cloneRootConstraints(constraints), sibling)
		}
		next := cloneRefSeen(seen)
		next[ref] = struct{}{}
		c.changed = true
		return c.walk(resolved, constraints, next, depth+1, unionDepth)
	}
	normalized, nullOnly, typeChanged := normalizeRootObjectType(schema)
	if nullOnly {
		c.changed = true
		return nil
	}
	if typeChanged {
		c.changed = true
	}
	keyword, branches, unionErr := rootUnion(normalized)
	if unionErr != nil {
		return invalidFunctionParametersRoot(c.context)
	}
	if keyword != "" {
		if c.rootUnionKeyword == "" {
			c.rootUnionKeyword = keyword
		} else {
			c.changed = true
			if c.rootUnionKeyword == "oneOf" || keyword != c.rootUnionKeyword {
				c.requiresDisjoint = true
			}
		}
		sibling, siblingErr := rootSchemaSiblingConstraint(normalized, keyword)
		if siblingErr != nil {
			return invalidFunctionParametersRoot(c.context)
		}
		if len(sibling) > 0 {
			constraints = append(cloneRootConstraints(constraints), sibling)
		}
		for _, branch := range branches {
			// Build function parameters may contain a nullable or otherwise
			// non-object alternative at the root. Such alternatives cannot be
			// represented by Build's grammar and are intentionally omitted; the
			// whole schema is rejected below when no object leaf remains.
			branchSchema, branchOK := branch.(map[string]any)
			if !branchOK || isNullOnlySchema(branchSchema) {
				c.changed = true
				continue
			}
			branchKeyword, _, branchErr := rootUnion(branchSchema)
			if branchErr != nil {
				return invalidFunctionParametersRoot(c.context)
			}
			// A root $ref may point to another union (for example a named
			// Create schema). Let walk resolve it before deciding whether its
			// leaves are object schemas.
			if branchKeyword == "" && branchSchema["$ref"] == nil && !isObjectRootSchema(branchSchema, c.doc, nil) {
				c.changed = true
				continue
			}
			if err := c.walk(branch, constraints, cloneRefSeen(seen), depth+1, unionDepth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if unionDepth == 0 && c.rootUnionKeyword != "" {
		return invalidFunctionParametersRoot(c.context)
	}
	if !isObjectRootSchema(normalized, c.doc, nil) {
		return invalidFunctionParametersRoot(c.context)
	}
	leaf := CloneJSONObject(normalized)
	if leaf["type"] != "object" {
		leaf["type"] = "object"
		c.changed = true
	}
	delete(leaf, "$defs")
	leaf = applyRootConstraints(leaf, constraints)
	c.leaves = append(c.leaves, leaf)
	if len(c.leaves) > maxRootUnionLeaves {
		return invalidFunctionParametersRoot(c.context)
	}
	return nil
}

func rootUnion(schema map[string]any) (string, []any, error) {
	var keyword string
	var branches []any
	for _, candidate := range []string{"anyOf", "oneOf"} {
		raw, exists := schema[candidate]
		if !exists {
			continue
		}
		if keyword != "" {
			return "", nil, fmt.Errorf("multiple root unions")
		}
		parsed, ok := raw.([]any)
		if !ok || len(parsed) == 0 {
			return "", nil, fmt.Errorf("invalid root union")
		}
		keyword, branches = candidate, parsed
	}
	return keyword, branches, nil
}

func normalizeRootObjectType(schema map[string]any) (map[string]any, bool, bool) {
	out := CloneJSONObject(schema)
	rawTypes, isArray := out["type"].([]any)
	if !isArray {
		return out, out["type"] == "null", false
	}
	filtered, removedNull := withoutNullSchemaTypes(rawTypes)
	if len(filtered) == 0 {
		return out, true, removedNull
	}
	for _, value := range filtered {
		if value != "object" {
			return out, false, false
		}
	}
	out["type"] = "object"
	return out, false, true
}

func rootSchemaSiblingConstraint(schema map[string]any, excluded string) (map[string]any, error) {
	constraint := make(map[string]any)
	for key, value := range schema {
		switch key {
		case excluded, "$defs":
			continue
		case "anyOf", "oneOf":
			return nil, fmt.Errorf("multiple root expressions")
		case "type":
			if value == "object" {
				continue
			}
		case "properties":
			if properties, ok := value.(map[string]any); ok && len(properties) == 0 {
				continue
			}
		case "required":
			if required, ok := value.([]any); ok && len(required) == 0 {
				continue
			}
		case "additionalProperties":
			if allowed, ok := value.(bool); ok && allowed {
				continue
			}
		}
		constraint[key] = CloneJSONValue(value)
	}
	return constraint, nil
}

func cloneRootConstraints(constraints []map[string]any) []map[string]any {
	return append([]map[string]any(nil), constraints...)
}

func applyRootConstraints(leaf map[string]any, constraints []map[string]any) map[string]any {
	if len(constraints) == 0 {
		return leaf
	}
	allOf := make([]any, 0, len(constraints))
	for _, constraint := range constraints {
		if len(constraint) > 0 {
			allOf = append(allOf, CloneJSONObject(constraint))
		}
	}
	allOf = append(allOf, leaf)
	return map[string]any{"type": "object", "allOf": allOf}
}

func cloneRefSeen(seen map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(seen))
	for key, value := range seen {
		out[key] = value
	}
	return out
}

func rootObjectLeavesPairwiseDisjoint(leaves []map[string]any, doc map[string]any) bool {
	for i := 0; i < len(leaves); i++ {
		for j := i + 1; j < len(leaves); j++ {
			if !rootObjectSchemasDisjoint(leaves[i], leaves[j], doc) {
				return false
			}
		}
	}
	return true
}

type rootObjectSchemaFacts struct {
	required map[string]struct{}
	values   map[string]map[string]struct{}
}

func rootObjectSchemasDisjoint(left, right map[string]any, doc map[string]any) bool {
	leftFacts := collectRootObjectSchemaFacts(left, doc, nil)
	rightFacts := collectRootObjectSchemaFacts(right, doc, nil)
	for property := range leftFacts.required {
		if _, required := rightFacts.required[property]; !required {
			continue
		}
		leftValues, leftKnown := leftFacts.values[property]
		rightValues, rightKnown := rightFacts.values[property]
		if leftKnown && rightKnown && finiteValueSetsDisjoint(leftValues, rightValues) {
			return true
		}
	}
	return false
}

func collectRootObjectSchemaFacts(schema map[string]any, doc map[string]any, seen map[string]struct{}) rootObjectSchemaFacts {
	facts := rootObjectSchemaFacts{required: map[string]struct{}{}, values: map[string]map[string]struct{}{}}
	collectRootObjectSchemaFactsInto(schema, doc, seen, &facts)
	return facts
}

func collectRootObjectSchemaFactsInto(schema map[string]any, doc map[string]any, seen map[string]struct{}, facts *rootObjectSchemaFacts) {
	if ref, ok := schema["$ref"].(string); ok {
		if _, loop := seen[ref]; !loop {
			if resolved, resolvedOK := ResolveLocalSchemaRef(doc, ref); resolvedOK {
				next := cloneRefSeen(seen)
				next[ref] = struct{}{}
				collectRootObjectSchemaFactsInto(resolved, doc, next, facts)
			}
		}
	}
	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			if property, ok := raw.(string); ok {
				facts.required[property] = struct{}{}
			}
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for property, raw := range properties {
			propertySchema, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			values, known := finiteSchemaValues(propertySchema, doc, nil)
			if !known {
				continue
			}
			if existing, exists := facts.values[property]; exists {
				facts.values[property] = intersectFiniteValueSets(existing, values)
			} else {
				facts.values[property] = values
			}
		}
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, raw := range allOf {
			if part, ok := raw.(map[string]any); ok {
				collectRootObjectSchemaFactsInto(part, doc, cloneRefSeen(seen), facts)
			}
		}
	}
}

func finiteSchemaValues(schema map[string]any, doc map[string]any, seen map[string]struct{}) (map[string]struct{}, bool) {
	if ref, ok := schema["$ref"].(string); ok {
		if _, loop := seen[ref]; loop {
			return nil, false
		}
		resolved, resolvedOK := ResolveLocalSchemaRef(doc, ref)
		if !resolvedOK {
			return nil, false
		}
		next := cloneRefSeen(seen)
		next[ref] = struct{}{}
		return finiteSchemaValues(resolved, doc, next)
	}
	if value, exists := schema["const"]; exists {
		key, ok := finiteSchemaValueKey(value)
		if !ok {
			return nil, false
		}
		return map[string]struct{}{key: {}}, true
	}
	if values, ok := schema["enum"].([]any); ok {
		result := make(map[string]struct{}, len(values))
		for _, value := range values {
			key, valid := finiteSchemaValueKey(value)
			if !valid {
				return nil, false
			}
			result[key] = struct{}{}
		}
		return result, true
	}
	return nil, false
}

func finiteSchemaValueKey(value any) (string, bool) {
	switch value.(type) {
	case nil, bool, float64, string:
		raw, err := json.Marshal(value)
		return string(raw), err == nil
	default:
		return "", false
	}
}

func finiteValueSetsDisjoint(left, right map[string]struct{}) bool {
	for value := range left {
		if _, exists := right[value]; exists {
			return false
		}
	}
	return true
}

func intersectFiniteValueSets(left, right map[string]struct{}) map[string]struct{} {
	intersection := make(map[string]struct{})
	for value := range left {
		if _, exists := right[value]; exists {
			intersection[value] = struct{}{}
		}
	}
	return intersection
}

func referencedDefs(node any, doc map[string]any) map[string]any {
	rawDefs, _ := doc["$defs"].(map[string]any)
	if len(rawDefs) == 0 {
		return nil
	}
	needed := map[string]struct{}{}
	collectDefRefs(node, needed)
	changed := true
	for changed {
		changed = false
		for name := range needed {
			extra := map[string]struct{}{}
			collectDefRefs(rawDefs[name], extra)
			for key := range extra {
				if _, exists := needed[key]; !exists {
					needed[key] = struct{}{}
					changed = true
				}
			}
		}
	}
	if len(needed) == 0 {
		return nil
	}
	out := make(map[string]any, len(needed))
	for name := range needed {
		if def, ok := rawDefs[name]; ok {
			out[name] = CloneJSONValue(def)
		}
	}
	return out
}

func collectDefRefs(node any, needed map[string]struct{}) {
	switch typed := node.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			if name, nameOK := rootDefinitionName(ref); nameOK {
				needed[name] = struct{}{}
			}
		}
		for _, value := range typed {
			collectDefRefs(value, needed)
		}
	case []any:
		for _, value := range typed {
			collectDefRefs(value, needed)
		}
	}
}

func rootDefinitionName(ref string) (string, bool) {
	tail := strings.TrimPrefix(ref, "#/$defs/")
	if tail == ref || tail == "" {
		return "", false
	}
	encoded := strings.SplitN(tail, "/", 2)[0]
	return strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~"), true
}

func withoutNullSchemaTypes(types []any) ([]any, bool) {
	filtered := make([]any, 0, len(types))
	removed := false
	for _, value := range types {
		if value == "null" {
			removed = true
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered, removed
}

func isNullOnlySchema(value any) bool {
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if schema["type"] == "null" {
		return true
	}
	types, ok := schema["type"].([]any)
	if !ok || len(types) == 0 {
		return false
	}
	for _, value := range types {
		if value != "null" {
			return false
		}
	}
	return true
}

func isObjectRootSchema(schema, root map[string]any, visited map[string]struct{}) bool {
	rawType, hasType := schema["type"]
	if rawType == "object" {
		return true
	}
	if types, ok := rawType.([]any); ok && len(types) > 0 {
		for _, value := range types {
			if value != "object" {
				return false
			}
		}
		return true
	}
	if hasType {
		return false
	}
	if _, hasProperties := schema["properties"]; hasProperties {
		return true
	}
	ref, ok := schema["$ref"].(string)
	if !ok {
		return false
	}
	if visited == nil {
		visited = make(map[string]struct{})
	}
	if _, seen := visited[ref]; seen {
		return false
	}
	resolved, ok := ResolveLocalSchemaRef(root, ref)
	if !ok {
		return false
	}
	visited[ref] = struct{}{}
	return isObjectRootSchema(resolved, root, visited)
}

// ResolveLocalSchemaRef follows a local JSON Pointer without expanding property recursion.
func ResolveLocalSchemaRef(root map[string]any, ref string) (map[string]any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = root
	for _, encoded := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		segment := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	resolved, ok := current.(map[string]any)
	return resolved, ok
}

func invalidFunctionParametersRoot(context functionParametersRootContext) error {
	message := context.param + " 顶层必须是非 nullable 的 object schema"
	if context.toolName != "" {
		message = fmt.Sprintf("工具 %s 的 %s 顶层必须是非 nullable 的 object schema", context.toolName, context.param)
	}
	return &FunctionParametersError{
		Message: message,
		Param:   context.param,
		Code:    "invalid_parameter",
	}
}
