package agent

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// UPS is the power supply as NUT reports it (upsc).
type UPS struct {
	Name       string   `json:"name"`
	Model      string   `json:"model,omitempty"`
	Status     string   `json:"status"` // raw ups.status, e.g. "OL", "OB DISCHRG", "OL CHRG LB"
	Flags      []string `json:"flags"`
	OnBattery  bool     `json:"onBattery"`
	LowBattery bool     `json:"lowBattery"`
	Charge     *float64 `json:"charge,omitempty"`  // %
	Runtime    *int     `json:"runtime,omitempty"` // seconds left
	Load       *float64 `json:"load,omitempty"`    // %
	Power      *float64 `json:"power,omitempty"`   // W, if the UPS reports it
	Nominal    *float64 `json:"nominalPower,omitempty"`
	InputV     *float64 `json:"inputVoltage,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func parseUpsc(out string) map[string]string {
	m := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func fnum(m map[string]string, k string) *float64 {
	if v, err := strconv.ParseFloat(m[k], 64); err == nil {
		return &v
	}
	return nil
}

// upsStatus reads the first UPS NUT knows about. nil = NUT isn't set up.
func (a *Agent) upsStatus(ctx context.Context) *UPS {
	list, err := a.cmd(ctx, 5*time.Second, "upsc", "-l", "localhost")
	if err != nil {
		return nil
	}
	// upsc also prints notices such as "Init SSL without certificate
	// database"; a UPS name is a single word.
	name := ""
	for line := range strings.SplitSeq(list, "\n") {
		if f := strings.Fields(line); len(f) == 1 {
			name = f[0]
			break
		}
	}
	if name == "" {
		return nil
	}
	u := &UPS{Name: name, Flags: []string{}}
	out, err := a.cmd(ctx, 5*time.Second, "upsc", name+"@localhost")
	if err != nil {
		msg := ""
		for line := range strings.SplitSeq(out, "\n") {
			if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "Init SSL") {
				msg = l
				break
			}
		}
		u.Error = "Can't read the UPS: " + msg
		return u
	}
	m := parseUpsc(out)
	u.Model = strings.TrimSpace(m["device.mfr"] + " " + m["device.model"])
	u.Status = m["ups.status"]
	u.Flags = strings.Fields(u.Status)
	for _, f := range u.Flags {
		switch f {
		case "OB":
			u.OnBattery = true
		case "LB":
			u.LowBattery = true
		}
	}
	u.Charge, u.Load = fnum(m, "battery.charge"), fnum(m, "ups.load")
	u.Power, u.Nominal, u.InputV = fnum(m, "ups.realpower"), fnum(m, "ups.realpower.nominal"), fnum(m, "input.voltage")
	if r := fnum(m, "battery.runtime"); r != nil {
		s := int(*r)
		u.Runtime = &s
	}
	if u.Power == nil && u.Load != nil && u.Nominal != nil {
		w := *u.Load / 100 * *u.Nominal
		u.Power = &w
	}
	return u
}
