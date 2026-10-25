package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// args is a small flag parser that allows flags before or after positional
// arguments, "--name value" and "--name=value". Unknown flags are errors.
type args struct {
	pos   []string
	vals  map[string]string
	bools map[string]bool
}

type flagSpec struct {
	values []string // flags that take a value
	bools  []string // flags that don't
}

func parseArgs(raw []string, spec flagSpec) (*args, error) {
	a := &args{vals: map[string]string{}, bools: map[string]bool{}}
	isValue := map[string]bool{}
	for _, v := range spec.values {
		isValue[v] = true
	}
	isBool := map[string]bool{"help": true, "pretty": true}
	for _, b := range spec.bools {
		isBool[b] = true
	}
	for i := 0; i < len(raw); i++ {
		arg := raw[i]
		if arg == "--" {
			a.pos = append(a.pos, raw[i+1:]...)
			break
		}
		if arg == "-h" {
			a.bools["help"] = true
			continue
		}
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			a.pos = append(a.pos, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg[2:], "=")
		switch {
		case isBool[name]:
			if hasValue {
				return nil, fmt.Errorf("--%s takes no value", name)
			}
			a.bools[name] = true
		case isValue[name]:
			if !hasValue {
				if i+1 >= len(raw) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				value = raw[i]
			}
			a.vals[name] = value
		default:
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
	}
	return a, nil
}

func (a *args) str(name string) string { return a.vals[name] }

func (a *args) has(name string) bool { return a.bools[name] }

func (a *args) int(name string, def int) (int, error) {
	v, ok := a.vals[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--%s must be a whole number, got %q", name, v)
	}
	return n, nil
}

// date accepts RFC 3339 times, plain dates (2026-10-23), and ages such as
// 7d, 12h or 30m.
func (a *args) date(name string, now time.Time) (time.Time, error) {
	v, ok := a.vals[name]
	if !ok {
		return time.Time{}, nil
	}
	return parseDate(v, now)
}

func parseDate(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, nil
	}
	if len(v) > 1 {
		n, err := strconv.Atoi(v[:len(v)-1])
		if err == nil && n >= 0 {
			switch v[len(v)-1] {
			case 'd':
				return now.Add(-time.Duration(n) * 24 * time.Hour), nil
			case 'h':
				return now.Add(-time.Duration(n) * time.Hour), nil
			case 'm':
				return now.Add(-time.Duration(n) * time.Minute), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("can't read %q as a date: use 2026-10-23, an RFC 3339 time, or an age such as 7d or 12h", v)
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
