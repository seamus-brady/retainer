package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// EnvPrefix is the namespace every config-override env var lives in.
// Combined with the toml tag path, it becomes the full env name —
// e.g. `RETAINER_COMMS_INBOX_ID` overrides `[comms].inbox_id`.
const EnvPrefix = "RETAINER"

// applyEnvOverrides walks the Config struct via reflection and
// applies any matching environment-variable overrides on top of
// what TOML loaded. Convention:
//
//   - Top-level scalar field with toml tag `foo` →
//     `RETAINER_FOO` overrides it.
//   - Nested struct with toml tag `bar` containing a field with
//     toml tag `baz` → `RETAINER_BAR_BAZ` overrides it.
//   - Arbitrary nesting works the same way (joined by `_`).
//
// Precedence: env > TOML > default. Env wins because it's the
// per-shell knob; TOML is the per-workspace baseline; defaults
// live at usage sites.
//
// Type support: *string, *int, *bool, *float64 (the four pointer
// flavours the schema uses), their non-pointer cousins, and
// []string. The string-slice support uses comma-separated values
// — fine for fields like `allowed_recipients` whose elements
// (email addresses) can't contain commas per RFC 5321. Slices of
// other element types stay TOML-only because comma-split semantics
// don't generalise (a comma in a model name or path is plausible).
//
// Errors: returns a non-nil error when an env var's value can't
// be parsed for its target type (e.g. `RETAINER_COG_MAX_TOOL_TURNS=banana`
// is a hard fail rather than a silent skip — the operator should
// know their override didn't take effect).
func applyEnvOverrides(c *Config) error {
	if c == nil {
		return nil
	}
	return applyEnvOverridesTo(reflect.ValueOf(c).Elem(), []string{EnvPrefix})
}

// applyEnvOverridesTo recurses into a struct value, building the
// env-var name from `prefix` joined with each field's toml tag.
// Unexported fields are skipped (reflect can't write to them
// anyway); fields without a toml tag are skipped (they aren't
// part of the public config surface).
func applyEnvOverridesTo(v reflect.Value, prefix []string) error {
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup("toml")
		if !ok || tag == "" || tag == "-" {
			continue
		}
		// toml tag may have options after a comma — strip them.
		if idx := strings.Index(tag, ","); idx >= 0 {
			tag = tag[:idx]
		}
		envSeg := strings.ToUpper(tag)
		fullPath := append(append([]string{}, prefix...), envSeg)
		envName := strings.Join(fullPath, "_")

		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			// Nested section — recurse with the extended prefix.
			if err := applyEnvOverridesTo(fv, fullPath); err != nil {
				return err
			}
		case reflect.Pointer:
			if err := applyPtrFromEnv(fv, envName); err != nil {
				return fmt.Errorf("env override %s: %w", envName, err)
			}
		case reflect.String, reflect.Int, reflect.Int64, reflect.Bool, reflect.Float64:
			if err := applyScalarFromEnv(fv, envName); err != nil {
				return fmt.Errorf("env override %s: %w", envName, err)
			}
		case reflect.Slice:
			// Only []string supports env override. Comma-split,
			// trim each element, drop empties. Other slice
			// element types stay TOML-only.
			if fv.Type().Elem().Kind() == reflect.String {
				if err := applyStringSliceFromEnv(fv, envName); err != nil {
					return fmt.Errorf("env override %s: %w", envName, err)
				}
			}
		default:
			// Anything else (map, interface, etc.) — leave alone.
		}
	}
	return nil
}

// applyPtrFromEnv allocates the target pointer's element if needed
// and writes the parsed env value into it. No-op when the env var
// is unset (preserves whatever TOML or default applied).
func applyPtrFromEnv(fv reflect.Value, envName string) error {
	raw, ok := os.LookupEnv(envName)
	if !ok {
		return nil
	}
	elemType := fv.Type().Elem()
	parsed, err := parseEnvValue(raw, elemType.Kind())
	if err != nil {
		return err
	}
	// Allocate fresh storage so we don't share aliasing with anything
	// the operator might have parked in TOML.
	ptr := reflect.New(elemType)
	ptr.Elem().Set(parsed)
	fv.Set(ptr)
	return nil
}

// applyStringSliceFromEnv writes a comma-split env value into a
// []string field. Whitespace around each element is trimmed; empty
// elements are dropped (so a trailing comma doesn't introduce an
// empty entry). An entirely empty value `FOO=` resolves to an
// empty slice — explicit "clear the allowlist via env" rather
// than "skip override".
func applyStringSliceFromEnv(fv reflect.Value, envName string) error {
	raw, ok := os.LookupEnv(envName)
	if !ok {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	fv.Set(reflect.ValueOf(out))
	return nil
}

// applyScalarFromEnv writes a parsed env value directly into a
// non-pointer field. Used for the (currently empty) set of plain
// scalar config fields; included for symmetry so adding one in
// future doesn't need a code change here.
func applyScalarFromEnv(fv reflect.Value, envName string) error {
	raw, ok := os.LookupEnv(envName)
	if !ok {
		return nil
	}
	parsed, err := parseEnvValue(raw, fv.Kind())
	if err != nil {
		return err
	}
	fv.Set(parsed)
	return nil
}

// parseEnvValue decodes raw into a reflect.Value of the requested
// kind. Empty string is rejected for bool/int/float because those
// have no sensible interpretation; for string it's just an empty
// string (an explicit clear).
func parseEnvValue(raw string, k reflect.Kind) (reflect.Value, error) {
	switch k {
	case reflect.String:
		return reflect.ValueOf(raw), nil
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("invalid int %q: %w", raw, err)
		}
		if k == reflect.Int {
			return reflect.ValueOf(int(n)), nil
		}
		return reflect.ValueOf(n), nil
	case reflect.Bool:
		// strconv.ParseBool accepts 1/0/t/f/T/F/true/false/True/False/TRUE/FALSE
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return reflect.Value{}, fmt.Errorf("invalid bool %q: %w", raw, err)
		}
		return reflect.ValueOf(b), nil
	case reflect.Float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("invalid float %q: %w", raw, err)
		}
		return reflect.ValueOf(f), nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported kind %s", k)
	}
}
