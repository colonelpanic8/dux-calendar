// Package external adapts calendar sources implemented as separate programs.
// The daemon spawns the configured command for each provider instance and
// speaks the JSON-RPC protocol described in docs/external-providers.md.
package external

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/duxlabs-dev/dux-calendar/core/internal/calendar"
)

const ProtocolVersion = 1

var errReadOnly = errors.New("external provider is read-only")

type capabilities struct {
	Events struct {
		Write bool `json:"write"`
	} `json:"events"`
	Tasks struct {
		Write bool `json:"write"`
	} `json:"tasks"`
}

type initializeParams struct {
	ProtocolVersion int              `json:"protocolVersion"`
	Account         calendar.Account `json:"account"`
}

type initializeResult struct {
	ProtocolVersion int          `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	Notices         []string     `json:"notices,omitempty"`
}

type syncParams struct {
	Calendar calendar.Calendar `json:"calendar"`
	Token    string            `json:"token,omitempty"`
}

type syncResult struct {
	Token             string                 `json:"token,omitempty"`
	Changes           []calendar.EventChange `json:"changes"`
	TaskChanges       []calendar.TaskChange  `json:"taskChanges,omitempty"`
	More              bool                   `json:"more"`
	FullSnapshot      bool                   `json:"fullSnapshot"`
	RetryAfterSeconds int                    `json:"retryAfterSeconds,omitempty"`
	Color             string                 `json:"color,omitempty"`
	Notices           []string               `json:"notices,omitempty"`
}

type listEventsParams struct {
	Calendar calendar.Calendar `json:"calendar"`
	Start    time.Time         `json:"start,omitzero"`
	End      time.Time         `json:"end,omitzero"`
	Limit    int               `json:"limit,omitempty"`
	Query    string            `json:"query,omitempty"`
}

type eventParams struct {
	Calendar calendar.Calendar `json:"calendar"`
	Event    calendar.Event    `json:"event"`
}

type taskParams struct {
	Calendar calendar.Calendar `json:"calendar"`
	Task     calendar.Task     `json:"task"`
}

type Provider struct {
	account calendar.Account
	client  *client
	caps    capabilities
	notices []string
}

// taskProvider is returned instead of Provider when the plugin can write
// tasks, so the calendar.TaskWriter type assertion reflects its capability.
type taskProvider struct {
	*Provider
}

func start(ctx context.Context, account calendar.Account, argv []string, env map[string]string) (calendar.Provider, error) {
	c, err := startClient(account.ID, argv, env)
	if err != nil {
		return nil, err
	}

	var init initializeResult
	if err := c.call(ctx, "initialize", initializeParams{ProtocolVersion: ProtocolVersion, Account: account}, &init); err != nil {
		c.close()
		return nil, mapErr(err)
	}
	if init.ProtocolVersion != ProtocolVersion {
		c.close()
		return nil, fmt.Errorf("external provider %s speaks protocol %d, want %d", account.ID, init.ProtocolVersion, ProtocolVersion)
	}

	p := &Provider{account: account, client: c, caps: init.Capabilities, notices: init.Notices}
	if p.caps.Tasks.Write {
		return taskProvider{p}, nil
	}
	return p, nil
}

func mapErr(err error) error {
	var re *rpcError
	if errors.As(err, &re) && re.Code == errCodeReauth {
		return fmt.Errorf("%w: %s", calendar.ErrReauthRequired, re.Message)
	}
	return err
}

func (p *Provider) call(ctx context.Context, method string, params, result any) error {
	return mapErr(p.client.call(ctx, method, params, result))
}

func (p *Provider) Kind() calendar.AccountKind { return calendar.AccountExternal }
func (p *Provider) Account() calendar.Account  { return p.account }
func (p *Provider) Notices() []string          { return p.notices }
func (p *Provider) Close() error {
	p.client.close()
	return nil
}

func (p *Provider) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	var res struct {
		Calendars []calendar.Calendar `json:"calendars"`
	}
	if err := p.call(ctx, "calendars.list", struct{}{}, &res); err != nil {
		return nil, err
	}
	for i := range res.Calendars {
		res.Calendars[i].AccountID = p.account.ID
		writable := p.caps.Events.Write
		if res.Calendars[i].HoldsTasks() {
			writable = p.caps.Tasks.Write
		}
		if !writable {
			res.Calendars[i].ReadOnly = true
		}
	}
	return res.Calendars, nil
}

func (p *Provider) Sync(ctx context.Context, cal calendar.Calendar, cursor calendar.SyncCursor) (*calendar.SyncResult, error) {
	var res syncResult
	if err := p.call(ctx, "calendar.sync", syncParams{Calendar: cal, Token: cursor.Token}, &res); err != nil {
		return nil, err
	}
	p.notices = append(p.notices, res.Notices...)

	changes := res.Changes[:0]
	for _, ch := range res.Changes {
		if ch.Type == calendar.ChangeUpsert {
			if ch.Event == nil {
				continue
			}
			ch.Event.CalendarID = cal.ID
		}
		changes = append(changes, ch)
	}
	taskChanges := res.TaskChanges[:0]
	for _, ch := range res.TaskChanges {
		if ch.Type == calendar.ChangeUpsert {
			if ch.Task == nil {
				continue
			}
			ch.Task.CalendarID = cal.ID
		}
		taskChanges = append(taskChanges, ch)
	}

	return &calendar.SyncResult{
		Cursor:       calendar.SyncCursor{CalendarID: cal.ID, Token: res.Token},
		Changes:      changes,
		TaskChanges:  taskChanges,
		More:         res.More,
		FullSnapshot: res.FullSnapshot,
		RetryAfter:   time.Duration(res.RetryAfterSeconds) * time.Second,
		Color:        res.Color,
	}, nil
}

func (p *Provider) ListEvents(ctx context.Context, cal calendar.Calendar, opts calendar.ListEventsOptions) ([]calendar.Event, error) {
	var res struct {
		Events []calendar.Event `json:"events"`
	}
	params := listEventsParams{Calendar: cal, Start: opts.Start, End: opts.End, Limit: opts.Limit, Query: opts.Query}
	if err := p.call(ctx, "events.list", params, &res); err != nil {
		return nil, err
	}
	for i := range res.Events {
		res.Events[i].CalendarID = cal.ID
	}
	return res.Events, nil
}

func (p *Provider) writeEvent(ctx context.Context, method string, cal calendar.Calendar, ev calendar.Event) (*calendar.Event, error) {
	if !p.caps.Events.Write {
		return nil, errReadOnly
	}
	var res struct {
		Event *calendar.Event `json:"event"`
	}
	if err := p.call(ctx, method, eventParams{Calendar: cal, Event: ev}, &res); err != nil {
		return nil, err
	}
	if res.Event == nil {
		return nil, fmt.Errorf("external provider %s: %s returned no event", p.account.ID, method)
	}
	res.Event.CalendarID = cal.ID
	return res.Event, nil
}

func (p *Provider) CreateEvent(ctx context.Context, cal calendar.Calendar, ev *calendar.Event) (*calendar.Event, error) {
	return p.writeEvent(ctx, "events.create", cal, *ev)
}

func (p *Provider) UpdateEvent(ctx context.Context, cal calendar.Calendar, ev *calendar.Event) (*calendar.Event, error) {
	return p.writeEvent(ctx, "events.update", cal, *ev)
}

func (p *Provider) DeleteEvent(ctx context.Context, cal calendar.Calendar, ev calendar.Event) error {
	if !p.caps.Events.Write {
		return errReadOnly
	}
	return p.call(ctx, "events.delete", eventParams{Calendar: cal, Event: ev}, nil)
}

func (p taskProvider) writeTask(ctx context.Context, method string, cal calendar.Calendar, t calendar.Task) (*calendar.Task, error) {
	var res struct {
		Task *calendar.Task `json:"task"`
	}
	if err := p.call(ctx, method, taskParams{Calendar: cal, Task: t}, &res); err != nil {
		return nil, err
	}
	if res.Task == nil {
		return nil, fmt.Errorf("external provider %s: %s returned no task", p.account.ID, method)
	}
	res.Task.CalendarID = cal.ID
	return res.Task, nil
}

func (p taskProvider) CreateTask(ctx context.Context, cal calendar.Calendar, t *calendar.Task) (*calendar.Task, error) {
	return p.writeTask(ctx, "tasks.create", cal, *t)
}

func (p taskProvider) UpdateTask(ctx context.Context, cal calendar.Calendar, t *calendar.Task) (*calendar.Task, error) {
	return p.writeTask(ctx, "tasks.update", cal, *t)
}

func (p taskProvider) DeleteTask(ctx context.Context, cal calendar.Calendar, t calendar.Task) error {
	return p.call(ctx, "tasks.delete", taskParams{Calendar: cal, Task: t}, nil)
}
