// Package notify delivers events to people through webhooks, chat apps,
// push services and email, with retries and a delivery log.
package notify

import (
	"strings"
	"time"

	"github.com/useless-husband/pharos/internal/i18n"
	"github.com/useless-husband/pharos/internal/model"
)

// Severity drives colors and priorities in channels that support them.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityGood
	SeverityWarning
	SeverityCritical
)

// Message is an event rendered for people.
type Message struct {
	Event    model.Event
	Title    string
	Body     string
	Link     string // dashboard URL of the monitor; empty without base_url
	Severity Severity
	Lang     string
	// Count is how many events the message reports. A grouped message
	// (Count > 1) carries the first of them in Event and links to the
	// dashboard overview.
	Count int
}

// Render turns an event into a message in lang, with times shown in loc.
func Render(ev model.Event, lang string, loc *time.Location, baseURL string) Message {
	name := ev.Monitor.Name
	msg := Message{Event: ev, Lang: lang, Count: 1}
	t := func(key string, args ...any) string { return i18n.T(lang, key, args...) }
	switch ev.Kind {
	case model.EventDown:
		msg.Severity = SeverityCritical
		msg.Title = t("notify.down.title", name)
		msg.Body = t("notify.down.body", name, ev.Message, i18n.FormatTime(ev.At, loc))
	case model.EventUp:
		if ev.Status == model.StatusMaintenance {
			msg.Severity = SeverityInfo
			msg.Title = t("notify.maint.title", name)
			msg.Body = t("notify.maint.body", name, ev.Message, i18n.Duration(lang, ev.Duration))
			break
		}
		msg.Severity = SeverityGood
		msg.Title = t("notify.up.title", name)
		msg.Body = t("notify.up.body", name, i18n.Duration(lang, ev.Duration))
	case model.EventDegraded:
		if ev.Status == model.StatusUp {
			msg.Severity = SeverityGood
			msg.Title = t("notify.perf_ok.title", name)
			msg.Body = t("notify.perf_ok.body", name)
		} else {
			msg.Severity = SeverityWarning
			msg.Title = t("notify.degraded.title", name)
			msg.Body = t("notify.degraded.body", name, ev.Message)
		}
	case model.EventReminder:
		msg.Severity = SeverityCritical
		msg.Title = t("notify.reminder.title", name)
		msg.Body = t("notify.reminder.body", name, i18n.Duration(lang, ev.Duration), ev.Message)
	case model.EventCert:
		msg.Severity = SeverityWarning
		msg.Title = t("notify.cert.title", name)
		msg.Body = t("notify.cert.body", ev.Monitor.Target, i18n.Duration(lang, ev.Duration.Round(time.Hour)), ev.CertExpiry.In(loc).Format("2006-01-02"))
	case model.EventTest:
		msg.Severity = SeverityInfo
		msg.Title = t("notify.test.title")
		msg.Body = t("notify.test.body", ev.Message)
	default:
		msg.Title, msg.Body = string(ev.Kind)+": "+name, ev.Message
	}
	if ev.Kind != model.EventTest && ev.Monitor.Target != "" && ev.Kind != model.EventCert {
		msg.Body += "\n" + t("notify.target", ev.Monitor.Target)
	}
	if baseURL != "" && ev.Monitor.ID != "" {
		msg.Link = baseURL + "/admin/monitors/" + ev.Monitor.ID
	}
	return msg
}

// maxGroupLines bounds a grouped message: chat services cap message length
// (Slack: 3,000 characters in a section).
const maxGroupLines = 20

// groupOrder is the order in which a grouped title counts events.
var groupOrder = []string{"down", "still_down", "degraded", "cert", "maint", "perf_ok", "up", "other"}

// RenderGroup renders events that happened close together as one message:
// a title counting them by kind ("Monitors: 12 down, 2 recovered") and one
// line per event, in the order they happened. One event renders as Render.
func RenderGroup(evs []model.Event, lang string, loc *time.Location, baseURL string) Message {
	if len(evs) == 1 {
		return Render(evs[0], lang, loc, baseURL)
	}
	t := func(key string, args ...any) string { return i18n.T(lang, key, args...) }
	msg := Message{Event: evs[0], Lang: lang, Count: len(evs)}
	counts := map[string]int{}
	var lines []string
	for i, ev := range evs {
		r := Render(ev, lang, loc, "")
		counts[groupKind(ev)]++
		msg.Severity = max(msg.Severity, r.Severity)
		if i < maxGroupLines {
			line := r.Title
			switch {
			case ev.Kind == model.EventUp && ev.Status != model.StatusMaintenance:
				line += " · " + t("notify.group.downtime", i18n.Duration(lang, ev.Duration))
			case ev.Kind == model.EventDown || ev.Kind == model.EventReminder || ev.Kind == model.EventDegraded && ev.Status != model.StatusUp:
				if ev.Message != "" {
					line += " · " + ev.Message
				}
			}
			lines = append(lines, "• "+truncate(line, 160))
		}
	}
	if n := len(evs) - maxGroupLines; n > 0 {
		lines = append(lines, t("notify.group.more", n))
	}
	var parts []string
	for _, k := range groupOrder {
		if counts[k] > 0 {
			parts = append(parts, t("notify.group."+k, counts[k]))
		}
	}
	msg.Title = t("notify.group.title", strings.Join(parts, t("notify.group.sep")))
	msg.Body = strings.Join(lines, "\n")
	if baseURL != "" {
		msg.Link = baseURL + "/admin"
	}
	return msg
}

func groupKind(ev model.Event) string {
	switch ev.Kind {
	case model.EventDown:
		return "down"
	case model.EventReminder:
		return "still_down"
	case model.EventDegraded:
		if ev.Status == model.StatusUp {
			return "perf_ok"
		}
		return "degraded"
	case model.EventCert:
		return "cert"
	case model.EventUp:
		if ev.Status == model.StatusMaintenance {
			return "maint"
		}
		return "up"
	}
	return "other"
}
