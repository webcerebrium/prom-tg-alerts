package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"prom_tg_alerts/internal/labels"
	"sort"
	"strings"
	"time"

	cli "github.com/jawher/mow.cli"
)

var app = cli.App("prom-tg-alerts", "Telegram Alerts Status notification for Prometheus")

var (
	prometheusAlertsURL = app.String(cli.StringOpt{
		Name:   "u url",
		Desc:   "Prometheus Alerts URL",
		EnvVar: "PROMETHEUS_ALERTS_URL",
		Value:  "",
	})
	tgBotToken = app.String(cli.StringOpt{
		Name:   "t tg-bot-token",
		Desc:   "Telegram Bot Token",
		EnvVar: "TELEGRAM_BOT_TOKEN",
		Value:  "",
	})
	tgChatId = app.String(cli.StringOpt{
		Name:   "c tg-chat-id",
		Desc:   "Telegram Chat ID",
		EnvVar: "TELEGRAM_CHAT_ID",
		Value:  "",
	})
	groupBy = app.String(cli.StringOpt{
		Name:   "g",
		Desc:   "Label to group summary messages",
		EnvVar: "GROUP_BY",
		Value:  "instance",
	})
	frequency = app.Int(cli.IntOpt{
		Name:   "f",
		Desc:   "Frequency of check in seconds",
		EnvVar: "FREQUENCY",
		Value:  15,
	})
)

// Alert is a generic representation of an alert in the Prometheus eco-system.
type Alert struct {
	// Label value pairs for purpose of aggregation, matching, and disposition
	// dispatching. This must minimally include an "alertname" label.
	Labels labels.Labels `json:"labels"`

	// Extra key/value information which does not define alert identity.
	Annotations labels.Labels `json:"annotations"`

	// The known time range for this alert. Both ends are optional.
	StartsAt     time.Time `json:"startsAt,omitempty"`
	EndsAt       time.Time `json:"endsAt,omitempty"`
	GeneratorURL string    `json:"generatorURL,omitempty"`
}

type alertState struct {
	Alerts []*Alert
	Error  string
}

func NewInitialState() *alertState {
	return &alertState{
		Alerts: make([]*Alert, 0),
		Error:  "",
	}
}

func NewAlertState(url string) *alertState {
	out := NewInitialState()
	res, err := http.Get(url)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	var data response
	if err := json.Unmarshal(body, &data); err != nil {
		log.Printf("prometheus response %v\n", string(body))
		out.Error = "Failed to get response from Prometheus"
		return out
	}

	if len(data.Error) > 0 {
		out.Error = data.Error
		return out
	}
	log.Printf("%v alerts from %v\n", len(data.Data.Alerts), url)
	out.Alerts = data.Data.Alerts
	return out
}

func (s *alertState) SortedAlerts() []*Alert {
	a := s.Alerts
	sort.SliceStable(s.Alerts, func(i, j int) bool {
		if a[i].StartsAt == a[j].StartsAt {
			return a[i].Labels.String() < a[j].Labels.String()
		}
		return a[i].StartsAt.Before(a[j].StartsAt)
	})
	return a
}

func niceGroup(input string) string {
	parts := strings.Split(input, ":")
	if len(parts) > 1 {
		return parts[0]
	}
	return input
}

func (s *alertState) Groupped() map[string][]*Alert {
	out := map[string][]*Alert{}
	for _, a := range s.SortedAlerts() {
		for _, l := range a.Labels {
			if l.Name == *groupBy {
				group := niceGroup(l.Value)
				if _, ok := out[group]; !ok {
					out[group] = make([]*Alert, 0)
				}
				out[group] = append(out[group], a)
			}
		}
	}
	return out
}

func alertToString(src *Alert) string {
	out := make([]string, 0)
	if src.Annotations != nil {
		summary := src.Annotations.Get("summary")
		if len(summary) > 0 {
			out = append(out, "• *"+summary+"*")
		}
		description := src.Annotations.Get("description")
		if len(description) > 0 {
			out = append(out, description)
		}
		if len(out) == 0 {
			out = append(out, src.Annotations.String())
		}
	}
	if len(out) == 0 {
		out = append(out, src.Labels.String())
	}
	return strings.Join(out, "\n")
}

const msgSizeLimit = 3500

func (s *alertState) Messages() map[string]string {
	out := map[string]string{}
	if s.Error != "" {
		out[""] = "ERROR: " + s.Error
	}
	for key, group := range s.Groupped() {
		msgSize := 0
		rows := make([]string, 0)
		for _, a := range group {
			if msgSize < msgSizeLimit {
				alert := alertToString(a)
				rows = append(rows, alert)
				msgSize += len(alert) + 2
			}
		}
		out[key] = strings.Join(rows, "\n")
		if len(out[key]) >= msgSizeLimit {
			out[key] = out[key] + "..."
		}
	}
	if len(out) == 0 {
		out[""] = "NO ALERTS"
	}
	return out
}

func (s *alertState) StateHash() string {
	out := ""
	if len(s.Error) > 0 {
		out += "&error=" + s.Error
	}
	for _, a := range s.SortedAlerts() {
		out += "&" + a.Labels.String()
	}
	return out
}

type responseData struct {
	Alerts []*Alert
}

type response struct {
	Status    string        `json:"status"`
	Data      *responseData `json:"data,omitempty"`
	ErrorType string        `json:"errorType,omitempty"`
	Error     string        `json:"error,omitempty"`
	Warnings  []string      `json:"warnings,omitempty"`
}

func rawurlencode(str string) string {
	return strings.Replace(url.QueryEscape(str), "+", "%20", -1)
}

func sendTelegram(from string, chatID string, body []byte) error {
	if len(body) > 3600 {
		return errors.New("message too long")
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage?chat_id=%s&parse_mode=Markdown&text=%s",
		from, chatID, rawurlencode(string(body)))
	log.Println("[URL]=", url)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	output, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = output
	log.Println("[OUT]=", string(output))
	return nil
}

func action() {
	prevState := NewInitialState()
	for {
		// read alertState from URL
		state := NewAlertState(*prometheusAlertsURL)
		if prevState.StateHash() != state.StateHash() {
			// build correct messages for each instance
			for key, msg := range state.Messages() {
				log.Printf("[MSG] %v=%v msg=%v", *groupBy, key, msg)
				// send message to telegram chat
				if err := sendTelegram(*tgBotToken, *tgChatId, []byte(msg)); err != nil {
					log.Println("[ERR] Notification failure", err)
				} else {
					log.Println("[OK] sent")
				}
			}
		}
		prevState = state
		time.Sleep(time.Duration(*frequency) * time.Second)
	}
}

func main() {
	app.Spec = "-u -t -c"
	app.Action = action
	if err := app.Run(os.Args); err != nil {
		log.Fatalln("[ERR]", err)
	}

}
