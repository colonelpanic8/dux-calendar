package external

import (
	"context"
	"fmt"

	"github.com/duxlabs-dev/dux-calendar/core/internal/calendar"
)

// Account settings keys.
const (
	SettingCommand = "command"
	SettingEnv     = "env"
)

type Factory struct{}

func (Factory) Kind() calendar.AccountKind { return calendar.AccountExternal }

func (Factory) Build(ctx context.Context, account calendar.Account, _ calendar.SecretStore) (calendar.Provider, error) {
	argv, err := stringList(account.Settings[SettingCommand])
	if err != nil {
		return nil, fmt.Errorf("external account %q: %s: %w", account.ID, SettingCommand, err)
	}
	env, err := stringMap(account.Settings[SettingEnv])
	if err != nil {
		return nil, fmt.Errorf("external account %q: %s: %w", account.ID, SettingEnv, err)
	}
	return start(ctx, account, argv, env)
}

func stringList(v any) ([]string, error) {
	switch v := v.(type) {
	case nil:
		return nil, fmt.Errorf("missing")
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected strings, got %T", item)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected a list of strings, got %T", v)
}

func stringMap(v any) (map[string]string, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case map[string]string:
		return v, nil
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("value for %q: expected string, got %T", k, item)
			}
			out[k] = s
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected an object of strings, got %T", v)
}
