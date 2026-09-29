// Package notify delivers events to people through webhooks, chat apps,
// push services and email, with retries and a delivery log.
package notify

import (
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
}

// Render turns an event into a message in lang, with times shown in loc.
func Render(ev model.Event, lang string, loc *time.Location, baseURL string) Message {
	name := ev.Monitor.Name
	msg := Message{Event: ev, Lang: lang}
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
