package external

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/duxlabs-dev/dux-calendar/core/internal/calendar"
)

const pluginModeEnv = "DCAL_EXTERNAL_TEST_PLUGIN"

// TestMain lets the test binary double as the plugin under test.
func TestMain(m *testing.M) {
	if mode := os.Getenv(pluginModeEnv); mode != "" {
		runFakePlugin(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakePlugin(mode string) {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		// An unsolicited notification must be skipped by the client.
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "log", "params": "hi"})

		reply := func(result any) { _ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}) }
		fail := func(code int, msg string) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": code, "message": msg}})
		}

		switch {
		case req.Method == "initialize" && mode == "reauth":
			fail(errCodeReauth, "bad password")
		case req.Method == "initialize":
			reply(map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"events": map[string]any{"write": false}, "tasks": map[string]any{"write": true}},
			})
		case mode == "crash":
			os.Exit(3)
		case req.Method == "calendars.list":
			reply(map[string]any{"calendars": []map[string]any{
				{"remoteId": "events", "name": "Schedule"},
				{"remoteId": "tasks", "name": "Tasks", "supportedComponents": []string{"VTODO"}},
			}})
		case req.Method == "calendar.sync":
			reply(map[string]any{
				"token":        "t1",
				"fullSnapshot": true,
				"changes": []map[string]any{
					{"type": "upsert", "event": map[string]any{"uid": "e1", "summary": "Standup", "start": "2026-11-02T09:00:00Z", "end": "2026-11-02T09:15:00Z"}},
					{"type": "upsert"},
				},
				"retryAfterSeconds": 30,
				"notices":           []string{"items_skipped"},
			})
		case req.Method == "tasks.update":
			var p taskParams
			_ = json.Unmarshal(req.Params, &p)
			p.Task.Status = calendar.TaskCompleted
			reply(map[string]any{"task": p.Task})
		default:
			fail(-32601, fmt.Sprintf("unknown method %s", req.Method))
		}
	}
}

func startFake(t *testing.T, mode string) (calendar.Provider, error) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	acc := calendar.Account{
		ID:   "external:test",
		Kind: calendar.AccountExternal,
		Settings: map[string]any{
			SettingCommand: []any{exe},
			SettingEnv:     map[string]any{pluginModeEnv: mode},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Factory{}.Build(ctx, acc, nil)
	if p != nil {
		t.Cleanup(func() { _ = p.Close() })
	}
	return p, err
}

func TestRoundTrip(t *testing.T) {
	p, err := startFake(t, "ok")
	require.NoError(t, err)
	ctx := context.Background()

	cals, err := p.ListCalendars(ctx)
	require.NoError(t, err)
	require.Len(t, cals, 2)
	assert.Equal(t, "external:test", cals[0].AccountID)
	assert.True(t, cals[0].ReadOnly, "event calendar is read-only without events.write")
	assert.False(t, cals[1].ReadOnly, "task calendar is writable with tasks.write")

	cal := calendar.Calendar{ID: "cal-1", RemoteID: "events"}
	res, err := p.Sync(ctx, cal, calendar.SyncCursor{})
	require.NoError(t, err)
	assert.Equal(t, calendar.SyncCursor{CalendarID: "cal-1", Token: "t1"}, res.Cursor)
	assert.True(t, res.FullSnapshot)
	assert.Equal(t, 30*time.Second, res.RetryAfter)
	require.Len(t, res.Changes, 1, "upserts without an event are dropped")
	assert.Equal(t, "cal-1", res.Changes[0].Event.CalendarID)
	assert.Equal(t, []string{"items_skipped"}, p.(calendar.NoticeReporter).Notices())

	_, err = p.CreateEvent(ctx, cal, &calendar.Event{})
	assert.ErrorIs(t, err, errReadOnly)

	writer, ok := p.(calendar.TaskWriter)
	require.True(t, ok)
	updated, err := writer.UpdateTask(ctx, calendar.Calendar{ID: "cal-2"}, &calendar.Task{UID: "t1"})
	require.NoError(t, err)
	assert.Equal(t, calendar.TaskCompleted, updated.Status)
	assert.Equal(t, "cal-2", updated.CalendarID)
}

func TestReauthError(t *testing.T) {
	_, err := startFake(t, "reauth")
	assert.True(t, errors.Is(err, calendar.ErrReauthRequired), err)
}

func TestPluginExitFailsCall(t *testing.T) {
	p, err := startFake(t, "crash")
	require.NoError(t, err)
	_, err = p.ListCalendars(context.Background())
	assert.ErrorContains(t, err, "exited")
}
