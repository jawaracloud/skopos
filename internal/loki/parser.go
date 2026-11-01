package loki

import (
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
)

// apply parses the body and merges extracted labels into labels. Parser errors
// leave the line in place with no extracted labels, as Loki does; use a label
// filter to drop lines that did not parse.
func (p ParserStage) apply(body string, labels map[string]string) bool {
	switch p.Kind {
	case "json":
		extracted := parseJSONBody(body, p.Extractions)
		mergeExtracted(labels, extracted)
	case "logfmt":
		mergeExtracted(labels, parseLogfmtBody(body))
	}
	return true
}

func parseJSONBody(body string, extractions []Extraction) map[string]string {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string)
	if len(extractions) == 0 {
		flattenJSON("", root, out)
		return out
	}
	for _, extraction := range extractions {
		value, ok := jsonPath(root, extraction.Path)
		if !ok {
			continue
		}
		if text, ok := jsonScalar(value); ok {
			out[extraction.Label] = text
		}
	}
	return out
}

// flattenJSON extracts scalar leaves. Nested objects are flattened with an
// underscore, matching Loki's | json parser. Arrays and null are skipped.
func flattenJSON(prefix string, value map[string]any, out map[string]string) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := value[key]
		name := NormalizeLabelName(key)
		if prefix != "" {
			name = prefix + "_" + name
		}
		switch v := child.(type) {
		case map[string]any:
			flattenJSON(name, v, out)
		case []any, nil:
			continue
		default:
			if text, ok := jsonScalar(v); ok {
				out[name] = text
			}
		}
	}
}

func jsonPath(root map[string]any, path []string) (any, bool) {
	var current any = root
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func jsonScalar(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), true
	default:
		return "", false
	}
}

func mergeExtracted(labels map[string]string, extracted map[string]string) {
	for name, value := range extracted {
		setExtracted(labels, name, value)
	}
}

// setExtracted follows Loki's collision rule: if a stream label already has a
// different value, the parsed value is stored under <name>_extracted.
func setExtracted(labels map[string]string, name, value string) {
	if existing, ok := labels[name]; !ok || existing == value {
		labels[name] = value
		return
	}
	candidate := name + "_extracted"
	for i := 2; ; i++ {
		if _, exists := labels[candidate]; !exists {
			labels[candidate] = value
			return
		}
		candidate = name + "_extracted_" + strconv.Itoa(i)
	}
}

// parseLogfmtBody is a small logfmt parser. It accepts key=value pairs
// separated by whitespace and double-quoted values with Go-style escapes.
// Unparseable tokens are skipped, matching Loki's non-strict default.
func parseLogfmtBody(body string) map[string]string {
	out := make(map[string]string)
	for i := 0; i < len(body); {
		for i < len(body) && isSpace(body[i]) {
			i++
		}
		if i >= len(body) {
			break
		}
		keyStart := i
		for i < len(body) && body[i] != '=' && !isSpace(body[i]) {
			i++
		}
		if i >= len(body) || body[i] != '=' {
			for i < len(body) && !isSpace(body[i]) {
				i++
			}
			continue
		}
		key := body[keyStart:i]
		i++
		var value string
		if i < len(body) && body[i] == '"' {
			start := i
			i++
			for i < len(body) {
				if body[i] == '\\' {
					i += 2
					continue
				}
				if body[i] == '"' {
					i++
					break
				}
				i++
			}
			quoted := body[start:i]
			if unquoted, err := strconv.Unquote(quoted); err == nil {
				value = unquoted
			} else {
				value = strings.Trim(quoted, `"`)
			}
		} else {
			valueStart := i
			for i < len(body) && !isSpace(body[i]) {
				if body[i] == '\\' && i+1 < len(body) {
					i += 2
					continue
				}
				i++
			}
			value = strings.ReplaceAll(body[valueStart:i], `\ `, " ")
		}
		if key == "" {
			continue
		}
		out[NormalizeLabelName(key)] = value
	}
	return out
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
