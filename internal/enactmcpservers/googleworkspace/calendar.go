package googleworkspace

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// calendarEventTime is Google's either/or: an all-day event carries `date`,
// a timed one carries `dateTime` plus a zone.
type calendarEventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type calendarEvent struct {
	ID          string             `json:"id"`
	Summary     string             `json:"summary"`
	Description string             `json:"description,omitempty"`
	Location    string             `json:"location,omitempty"`
	Start       *calendarEventTime `json:"start,omitempty"`
	End         *calendarEventTime `json:"end,omitempty"`
	Attendees   []struct {
		Email          string `json:"email"`
		ResponseStatus string `json:"responseStatus"`
	} `json:"attendees,omitempty"`
	HTMLLink string `json:"htmlLink,omitempty"`
	Status   string `json:"status,omitempty"`
}

type listCalendarsArgs struct{}

type getEventsArgs struct {
	CalendarID string `json:"calendar_id,omitempty" jsonschema:"which calendar (default 'primary')"`
	TimeMin    string `json:"time_min,omitempty" jsonschema:"earliest start, RFC3339 (e.g. 2026-08-18T00:00:00Z); defaults to now"`
	TimeMax    string `json:"time_max,omitempty" jsonschema:"latest start, RFC3339; defaults to 7 days after time_min"`
	Query      string `json:"query,omitempty" jsonschema:"free-text search across the event fields"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"how many events to return (default 20, max 100)"`
}

type createEventArgs struct {
	CalendarID  string   `json:"calendar_id,omitempty" jsonschema:"which calendar (default 'primary')"`
	Summary     string   `json:"summary" jsonschema:"the event title"`
	Start       string   `json:"start" jsonschema:"start time, RFC3339 for a timed event or YYYY-MM-DD for an all-day one"`
	End         string   `json:"end" jsonschema:"end time, same format as start"`
	Description string   `json:"description,omitempty" jsonschema:"longer description"`
	Location    string   `json:"location,omitempty" jsonschema:"where it happens"`
	Attendees   []string `json:"attendees,omitempty" jsonschema:"email addresses to invite"`
	TimeZone    string   `json:"time_zone,omitempty" jsonschema:"IANA zone for a timed event (e.g. Europe/Bucharest); defaults to the calendar's"`
}

type modifyEventArgs struct {
	CalendarID  string   `json:"calendar_id,omitempty" jsonschema:"which calendar (default 'primary')"`
	EventID     string   `json:"event_id" jsonschema:"the event to change"`
	Summary     string   `json:"summary,omitempty" jsonschema:"new title; omit to leave unchanged"`
	Start       string   `json:"start,omitempty" jsonschema:"new start; omit to leave unchanged"`
	End         string   `json:"end,omitempty" jsonschema:"new end; omit to leave unchanged"`
	Description string   `json:"description,omitempty" jsonschema:"new description; omit to leave unchanged"`
	Location    string   `json:"location,omitempty" jsonschema:"new location; omit to leave unchanged"`
	Attendees   []string `json:"attendees,omitempty" jsonschema:"replace the attendee list with these addresses"`
	TimeZone    string   `json:"time_zone,omitempty" jsonschema:"IANA zone for the new times"`
}

type deleteEventArgs struct {
	CalendarID string `json:"calendar_id,omitempty" jsonschema:"which calendar (default 'primary')"`
	EventID    string `json:"event_id" jsonschema:"the event to delete"`
}

func registerCalendar(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name:        "list_calendars",
		Description: "List the calendars the user can see, with their ids — needed to address any calendar other than 'primary'.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ listCalendarsArgs) (string, any, error) {
		var listing struct {
			Items []struct {
				ID          string `json:"id"`
				Summary     string `json:"summary"`
				Description string `json:"description"`
				Primary     bool   `json:"primary"`
				AccessRole  string `json:"accessRole"`
				TimeZone    string `json:"timeZone"`
			} `json:"items"`
		}
		if err := c.get(ctx, calendarBase+"/users/me/calendarList", nil, &listing); err != nil {
			return "", nil, err
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "%d calendar(s):\n", len(listing.Items))
		for _, cal := range listing.Items {
			marker := ""
			if cal.Primary {
				marker = " [primary]"
			}
			fmt.Fprintf(&summary, "- %s (id %s, %s, %s)%s\n", cal.Summary, cal.ID, cal.AccessRole, cal.TimeZone, marker)
		}
		return summary.String(), map[string]any{"calendars": listing.Items}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_events",
		Description: "List events in a time range, ordered by start. Use it to answer questions about what is on the user's calendar.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getEventsArgs) (string, any, error) {
		calendarID := defaultString(args.CalendarID, "primary")
		max := args.MaxResults
		if max <= 0 {
			max = 20
		}
		if max > 100 {
			max = 100
		}
		timeMin := args.TimeMin
		if timeMin == "" {
			timeMin = time.Now().UTC().Format(time.RFC3339)
		}
		timeMax := args.TimeMax
		if timeMax == "" {
			// A week is the span most "what's coming up" questions mean, and
			// an unbounded query on a busy calendar is a large answer.
			if parsed, err := time.Parse(time.RFC3339, timeMin); err == nil {
				timeMax = parsed.AddDate(0, 0, 7).Format(time.RFC3339)
			}
		}
		query := url.Values{}
		query.Set("timeMin", timeMin)
		if timeMax != "" {
			query.Set("timeMax", timeMax)
		}
		query.Set("maxResults", strconv.Itoa(max))
		query.Set("singleEvents", "true")
		query.Set("orderBy", "startTime")
		if strings.TrimSpace(args.Query) != "" {
			query.Set("q", args.Query)
		}

		var listing struct {
			Items []calendarEvent `json:"items"`
		}
		if err := c.get(ctx, calendarBase+"/calendars/"+url.PathEscape(calendarID)+"/events", query, &listing); err != nil {
			return "", nil, err
		}
		if len(listing.Items) == 0 {
			return fmt.Sprintf("No events on %s between %s and %s.", calendarID, timeMin, timeMax),
				map[string]any{"events": []any{}}, nil
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "%d event(s) on %s:\n", len(listing.Items), calendarID)
		for _, e := range listing.Items {
			fmt.Fprintf(&summary, "- %s — %s to %s (id %s)%s\n",
				e.Summary, eventTime(e.Start), eventTime(e.End), e.ID,
				optional(" at ", e.Location))
		}
		return summary.String(), map[string]any{"events": listing.Items}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "create_event",
		Description: "Create a calendar event. Invitations are sent to any attendees given.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createEventArgs) (string, any, error) {
		if strings.TrimSpace(args.Summary) == "" || strings.TrimSpace(args.Start) == "" || strings.TrimSpace(args.End) == "" {
			return "", nil, fmt.Errorf("summary, start and end are required")
		}
		calendarID := defaultString(args.CalendarID, "primary")
		event := calendarEvent{
			Summary:     args.Summary,
			Description: args.Description,
			Location:    args.Location,
			Start:       toEventTime(args.Start, args.TimeZone),
			End:         toEventTime(args.End, args.TimeZone),
		}
		body := map[string]any{
			"summary": event.Summary, "description": event.Description,
			"location": event.Location, "start": event.Start, "end": event.End,
		}
		if len(args.Attendees) > 0 {
			attendees := make([]map[string]string, 0, len(args.Attendees))
			for _, a := range args.Attendees {
				attendees = append(attendees, map[string]string{"email": a})
			}
			body["attendees"] = attendees
		}
		var created calendarEvent
		endpoint := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events"
		if len(args.Attendees) > 0 {
			endpoint += "?sendUpdates=all"
		}
		if err := c.post(ctx, endpoint, body, &created); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Created %q on %s from %s to %s (id %s).",
				created.Summary, calendarID, eventTime(created.Start), eventTime(created.End), created.ID),
			created, nil
	})

	addTool(server, &mcp.Tool{
		Name: "modify_event",
		Description: "Change an existing event. Only the fields given are altered — " +
			"note that supplying attendees REPLACES the whole list.",
		Annotations: mutating(true),
	}, func(ctx context.Context, args modifyEventArgs) (string, any, error) {
		if strings.TrimSpace(args.EventID) == "" {
			return "", nil, fmt.Errorf("event_id is required")
		}
		calendarID := defaultString(args.CalendarID, "primary")
		// PATCH, not PUT: a partial update leaves untouched fields alone,
		// where a full replace would silently clear everything not sent.
		body := map[string]any{}
		if args.Summary != "" {
			body["summary"] = args.Summary
		}
		if args.Description != "" {
			body["description"] = args.Description
		}
		if args.Location != "" {
			body["location"] = args.Location
		}
		if args.Start != "" {
			body["start"] = toEventTime(args.Start, args.TimeZone)
		}
		if args.End != "" {
			body["end"] = toEventTime(args.End, args.TimeZone)
		}
		if len(args.Attendees) > 0 {
			attendees := make([]map[string]string, 0, len(args.Attendees))
			for _, a := range args.Attendees {
				attendees = append(attendees, map[string]string{"email": a})
			}
			body["attendees"] = attendees
		}
		if len(body) == 0 {
			return "", nil, fmt.Errorf("give at least one field to change")
		}
		var updated calendarEvent
		endpoint := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(args.EventID)
		if err := c.patch(ctx, endpoint, body, &updated); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Updated %q (id %s): %s to %s.",
			updated.Summary, updated.ID, eventTime(updated.Start), eventTime(updated.End)), updated, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "delete_event",
		Description: "Delete a calendar event. This cannot be undone.",
		Annotations: mutating(true),
	}, func(ctx context.Context, args deleteEventArgs) (string, any, error) {
		if strings.TrimSpace(args.EventID) == "" {
			return "", nil, fmt.Errorf("event_id is required")
		}
		calendarID := defaultString(args.CalendarID, "primary")
		endpoint := calendarBase + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(args.EventID)
		if err := c.del(ctx, endpoint); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Deleted event %s from %s.", args.EventID, calendarID),
			map[string]any{"deleted": args.EventID}, nil
	})
}

// toEventTime picks the right half of Google's either/or shape: a bare date
// is an all-day event, anything else is a timestamp.
func toEventTime(value, zone string) *calendarEventTime {
	if len(value) == 10 && strings.Count(value, "-") == 2 {
		return &calendarEventTime{Date: value}
	}
	return &calendarEventTime{DateTime: value, TimeZone: zone}
}

// eventTime renders whichever half is set.
func eventTime(t *calendarEventTime) string {
	if t == nil {
		return "?"
	}
	if t.Date != "" {
		return t.Date + " (all day)"
	}
	return t.DateTime
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func optional(prefix, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return prefix + value
}
