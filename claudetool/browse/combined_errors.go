package browse

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

const browserExamples = `Examples: {"action":"navigate","url":"https://example.com"}, {"action":"eval","expression":"document.title"}.`

// browserShapeError explains why raw is not a JSON object and how to resend it.
func browserShapeError(raw json.RawMessage) error {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "null":
		return fmt.Errorf("invalid browser call: arguments were null, expected a JSON object. Retry with an object that sets \"action\". %s", browserExamples)
	case strings.HasPrefix(trimmed, "["):
		return fmt.Errorf("invalid browser call: arguments were a JSON array, expected a single JSON object. Make one browser call per action. %s", browserExamples)
	case strings.HasPrefix(trimmed, `"`):
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			break
		}
		var object map[string]json.RawMessage
		err := json.Unmarshal([]byte(text), &object)
		if err == nil && object != nil {
			return fmt.Errorf("invalid browser call: arguments were sent as a quoted JSON string instead of a JSON object. Decoded arguments: %s. Resend an unquoted JSON object with a non-empty string \"action\" and field types matching the browser schema.", text)
		}
		var syntax *json.SyntaxError
		detail := "it is not a JSON object"
		if errors.As(err, &syntax) {
			detail = fmt.Sprintf("JSON syntax error at byte %d: %v", syntax.Offset, err)
		} else if err != nil {
			detail = err.Error()
		}
		return fmt.Errorf("invalid browser call: arguments were not valid JSON (%s). Received: %s. Resend one complete JSON object with double-quoted keys and strings and balanced braces. %s", detail, truncate(text, 300), browserExamples)
	}
	return fmt.Errorf("invalid browser call: arguments were not a JSON object. Received: %s. %s", truncate(trimmed, 300), browserExamples)
}

// browserFieldError explains a field whose JSON type does not match the schema.
func browserFieldError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) || typeErr.Field == "" {
		return fmt.Errorf("invalid browser call: %v. Use the field types in the browser schema: strings quoted, numbers unquoted, booleans true/false. %s", err, browserExamples)
	}
	field := typeErr.Field[strings.LastIndex(typeErr.Field, ".")+1:]
	return fmt.Errorf("invalid browser call: field %q must be %s, got a JSON %s. Resend the same call with %q as %s.", field, jsonTypeName(typeErr.Type), typeErr.Value, field, jsonExample(typeErr.Type))
}

func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int64:
		return "an integer"
	case reflect.Float64:
		return "a number"
	case reflect.String:
		return "a string"
	case reflect.Pointer:
		return jsonTypeName(t.Elem())
	}
	return t.String()
}

func jsonExample(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "true or false (unquoted)"
	case reflect.Int, reflect.Int64:
		return "a whole number (unquoted, e.g. 1280)"
	case reflect.Float64:
		return "a number (unquoted, e.g. 2)"
	case reflect.String:
		return `a quoted string`
	case reflect.Pointer:
		return jsonExample(t.Elem())
	}
	return jsonTypeName(t)
}

func missingActionError() error {
	return fmt.Errorf(`invalid browser call: "action" is required and must be a non-empty string. Choose an action from the browser schema and resend the call. %s`, browserExamples)
}

func unknownActionError(action string) error {
	return fmt.Errorf("invalid browser call: unknown action %q. Choose an action from the browser schema and resend the call. All supported actions: %s.", action, strings.Join(combinedBrowserActions, ", "))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
